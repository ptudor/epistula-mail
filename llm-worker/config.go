package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

var defaultConfigPaths = []string{
	"/usr/local/etc/epistula/epistula-llm-worker.toml",
	"/etc/epistula/epistula-llm-worker.toml",
	"epistula-llm-worker.toml",
}

type Config struct {
	Production bool           `toml:"production"`
	MailAPI    MailAPIConfig  `toml:"mail_api"`
	LMStudio   LMStudioConfig `toml:"lmstudio"`
	Worker     WorkerConfig   `toml:"worker"`
	Classify   ClassifyConfig `toml:"classify"`
	Admin      AdminConfig    `toml:"admin"`
	Logging    LoggingConfig  `toml:"logging"`
}

type MailAPIConfig struct {
	BaseURL           string `toml:"base_url"`
	Token             string `toml:"token"`
	Mailbox           string `toml:"mailbox"`
	Folder            string `toml:"folder"`
	Since             string `toml:"since"`
	Before            string `toml:"before"`
	Query             string `toml:"query"`
	Tag               string `toml:"tag"`
	Category          string `toml:"category"`
	RequestTimeoutSec int    `toml:"request_timeout_seconds"`
}

type LMStudioConfig struct {
	BaseURL           string  `toml:"base_url"`
	APIToken          string  `toml:"api_token"`
	Model             string  `toml:"model"`
	Temperature       float64 `toml:"temperature"`
	MaxTokens         int     `toml:"max_tokens"`
	RequestTimeoutSec int     `toml:"request_timeout_seconds"`
	// ReasoningEffort is sent as the request's reasoning_effort when set.
	// "none" turns a hybrid reasoning model's thinking off (Qwen3 in LM
	// Studio honours it; chat_template_kwargs and "/no_think" did not help),
	// so the answer comes straight back as content and cannot run out of
	// max_tokens mid-thought. Empty sends nothing: the model's default.
	ReasoningEffort string `toml:"reasoning_effort"`
}

type WorkerConfig struct {
	// IntervalSec is the pause between rounds. Most rounds are fast rounds,
	// which read only the messages queued for the pipeline
	// (annotation_pass_required, epistula-database migration 022) and so cost
	// what is queued rather than what is stored.
	IntervalSec int `toml:"interval_seconds"`
	// CompleteRoundIntervalSec is how often a round is instead a complete
	// round: the full sweep for this model's unannotated and unclassified
	// mail, which finds what no insert queues (a new annotation model, a
	// retired category, mail stored before the queue existed, a message whose
	// fast-round retries were deferred). The first round after start is
	// always complete. 0 makes every round complete, the behaviour before the
	// queue; so does a epistula-api without the queue, until it has one.
	CompleteRoundIntervalSec int `toml:"complete_round_interval_seconds"`

	Concurrency          int      `toml:"concurrency"`
	BatchLimit           int      `toml:"batch_limit"`
	SkipAnnotated        bool     `toml:"skip_annotated"`
	AnnotationModel      string   `toml:"annotation_model"`
	MaxBodyChars         int      `toml:"max_body_chars"`
	MaxSummaryChars      int      `toml:"max_summary_chars"`
	MaxTags              int      `toml:"max_tags"`
	Categories           []string `toml:"categories"`
	AllowedTags          []string `toml:"allowed_tags"`
	RetryAttempts        int      `toml:"retry_attempts"`
	RetryBackoff         string   `toml:"retry_backoff"`
	DryRun               bool     `toml:"dry_run"`
	ExitOnMessageFailure bool     `toml:"exit_on_message_failure"`
}

// ClassifyConfig turns on archive classification (ARCHIVE_SORTING.md): for a
// mailbox whose archive categories are set up in epistula-database, every
// annotation also chooses one of them, and messages this model annotated
// before classification was on are classified in a second pass. The epistula-api
// token then needs write_classification as well as write_annotation.
type ClassifyConfig struct {
	Enabled bool `toml:"enabled"`
	// MaxBodyChars bounds the message text a classification-only request
	// sends. A combined request uses worker.max_body_chars.
	MaxBodyChars int `toml:"max_body_chars"`
}

type AdminConfig struct {
	ListenAddr         string `toml:"listen_addr"`
	ExposeMetrics      bool   `toml:"expose_metrics"`
	ReadTimeoutSec     int    `toml:"read_timeout_seconds"`
	WriteTimeoutSec    int    `toml:"write_timeout_seconds"`
	ShutdownTimeoutSec int    `toml:"shutdown_timeout_seconds"`
}

type LoggingConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// epistula-api annotation-contract limits (mirror api
// annotation.go). The worker must produce values within these or every PUT is
// rejected 422 and the message is re-annotated forever (R-033).
const (
	apiMaxTagBytes      = 128
	apiMaxTagCount      = 64
	apiMaxModelBytes    = 128
	apiMaxCategoryBytes = 256
	// apiMaxAnnotationBytes is the worker's own bound on the PUT body. It
	// sits below epistula-api's limits.max_annotation_bytes default (256 KiB),
	// so the API accepts every body the worker sends unless an operator lowers
	// that limit under 64 KiB. The PUT body is JSON, so a summary of N
	// characters can serialize to several times N bytes once multibyte runes
	// and escapes are counted — which is why the summary limit is enforced
	// against the SERIALIZED body and not against a character count (RA6X-044).
	apiMaxAnnotationBytes = 64 * 1024
)

func DefaultConfig() *Config {
	return &Config{
		Production: false,
		MailAPI: MailAPIConfig{
			BaseURL:           "https://epistula-api.example.invalid",
			RequestTimeoutSec: 300,
		},
		LMStudio: LMStudioConfig{
			BaseURL:           "http://127.0.0.1:1234/v1",
			Model:             "qwen/qwen3.6-35b-a3b",
			Temperature:       0.1,
			MaxTokens:         900,
			RequestTimeoutSec: 300,
		},
		Worker: WorkerConfig{
			IntervalSec:              900,
			CompleteRoundIntervalSec: 6 * 60 * 60,
			Concurrency:              1,
			BatchLimit:               0,
			SkipAnnotated:            true,
			// Empty so the post-load derivation in LoadConfig sets it to
			// "lmstudio:" + lmstudio.model — an operator who only edits
			// lmstudio.model (per QUICKSTART) gets a matching annotation key.
			// A non-empty default here would make the derivation dead code and
			// silently label every box's annotations with the qwen key (R-011).
			// An explicit TOML annotation_model still wins (derivation is
			// only-if-empty).
			AnnotationModel:      "",
			MaxBodyChars:         24000,
			MaxSummaryChars:      1400,
			MaxTags:              8,
			Categories:           []string{"action", "waiting", "reference", "receipt", "newsletter", "personal", "finance", "travel", "security", "spam", "other"},
			AllowedTags:          []string{},
			RetryAttempts:        2,
			RetryBackoff:         "2s",
			ExitOnMessageFailure: false,
		},
		Classify: ClassifyConfig{
			Enabled:      false,
			MaxBodyChars: 6000,
		},
		Admin: AdminConfig{
			ListenAddr:         "127.0.0.1:8790",
			ExposeMetrics:      true,
			ReadTimeoutSec:     5,
			WriteTimeoutSec:    10,
			ShutdownTimeoutSec: 30,
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		if err := loadTOML(path, cfg); err != nil {
			return nil, err
		}
	} else {
		var found string
		for _, p := range defaultConfigPaths {
			if _, err := os.Stat(p); err == nil {
				found = p
				break
			}
		}
		if found != "" {
			if err := loadTOML(found, cfg); err != nil {
				return nil, err
			}
		}
	}
	applyEnv(cfg)
	cfg.deriveAnnotationModel()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// deriveAnnotationModel fills Worker.AnnotationModel from lmstudio.model when
// it was not set explicitly (empty), producing "lmstudio:<model>". Called by
// LoadConfig before Validate so an operator who edits only lmstudio.model gets
// a matching annotation key; an explicit annotation_model (and thus a value
// pinned in TOML) always wins because the derivation is only-if-empty. See
// R-011.
func (c *Config) deriveAnnotationModel() {
	if c.Worker.AnnotationModel == "" && c.LMStudio.Model != "" {
		c.Worker.AnnotationModel = "lmstudio:" + c.LMStudio.Model
	}
	// Canonicalize the model identity ONCE, here, so every use of it agrees
	// (RA6X-044).
	//
	// epistula-api trims the model on write. The worker sent the untrimmed string
	// and then compared the untrimmed string against what came back, so an
	// annotation_model configured with a stray space or newline was stored
	// under the trimmed name, never matched the skip check, and the whole
	// corpus was re-annotated on every single pass — an expensive, silent
	// loop. Trimming at load time makes the value the worker queries with, the
	// value it compares with, and the value it writes the same string.
	c.Worker.AnnotationModel = strings.TrimSpace(c.Worker.AnnotationModel)
}

func loadTOML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MAIL_LLM_MAIL_API_URL"); v != "" {
		cfg.MailAPI.BaseURL = v
	}
	if v := os.Getenv("MAIL_LLM_MAIL_API_TOKEN"); v != "" {
		cfg.MailAPI.Token = v
	}
	if v := os.Getenv("MAIL_LLM_LMSTUDIO_URL"); v != "" {
		cfg.LMStudio.BaseURL = v
	}
	if v := os.Getenv("MAIL_LLM_LMSTUDIO_TOKEN"); v != "" {
		cfg.LMStudio.APIToken = v
	}
	if v := os.Getenv("MAIL_LLM_MODEL"); v != "" {
		cfg.LMStudio.Model = v
		// Do NOT clobber annotation_model here: if TOML pinned it, that value
		// must win. The post-load derivation in LoadConfig sets
		// AnnotationModel from this (env-overridden) model only when TOML left
		// it empty (R-011).
	}
	if v := os.Getenv("MAIL_LLM_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("MAIL_LLM_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv("MAIL_LLM_PRODUCTION"); v == "true" || v == "1" {
		cfg.Production = true
	}
}

func (c *Config) Validate() error {
	if _, err := parseBaseURL(c.MailAPI.BaseURL); err != nil {
		return fmt.Errorf("mail_api.base_url: %w", err)
	}
	if strings.TrimSpace(c.MailAPI.Token) == "" {
		return errors.New("mail_api.token is required")
	}
	if c.MailAPI.RequestTimeoutSec <= 0 {
		return errors.New("mail_api.request_timeout_seconds must be positive")
	}
	if _, err := parseBaseURL(c.LMStudio.BaseURL); err != nil {
		return fmt.Errorf("lmstudio.base_url: %w", err)
	}
	if strings.TrimSpace(c.LMStudio.Model) == "" {
		return errors.New("lmstudio.model is required")
	}
	if c.LMStudio.Temperature < 0 || c.LMStudio.Temperature > 2 {
		return errors.New("lmstudio.temperature must be between 0 and 2")
	}
	if c.LMStudio.MaxTokens <= 0 {
		return errors.New("lmstudio.max_tokens must be positive")
	}
	switch c.LMStudio.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high":
	default:
		return fmt.Errorf("lmstudio.reasoning_effort must be empty or one of none, minimal, low, medium, high: %q", c.LMStudio.ReasoningEffort)
	}
	if c.LMStudio.RequestTimeoutSec <= 0 {
		return errors.New("lmstudio.request_timeout_seconds must be positive")
	}
	if c.Worker.IntervalSec < 0 {
		return errors.New("worker.interval_seconds must be non-negative")
	}
	if c.Worker.CompleteRoundIntervalSec < 0 {
		return errors.New("worker.complete_round_interval_seconds must be non-negative")
	}
	if c.Worker.Concurrency != 1 {
		return errors.New("worker.concurrency must be 1 for v1; LM Studio single-GPU scheduling is intentionally serialized")
	}
	if c.Worker.BatchLimit < 0 {
		return errors.New("worker.batch_limit must be non-negative")
	}
	if strings.TrimSpace(c.Worker.AnnotationModel) == "" {
		return errors.New("worker.annotation_model is required")
	}
	if c.Worker.MaxBodyChars <= 0 {
		return errors.New("worker.max_body_chars must be positive")
	}
	if c.Worker.MaxSummaryChars <= 0 {
		return errors.New("worker.max_summary_chars must be positive")
	}
	if c.Worker.MaxTags <= 0 {
		return errors.New("worker.max_tags must be positive")
	}
	// Stay within the epistula-api annotation contract or every PUT is rejected
	// 422, permanently re-processing the message every pass (R-033).
	if c.Worker.MaxTags > apiMaxTagCount {
		return fmt.Errorf("worker.max_tags must be <= %d (epistula-api annotation contract)", apiMaxTagCount)
	}
	if len(c.Worker.AnnotationModel) > apiMaxModelBytes {
		return fmt.Errorf("worker.annotation_model must be <= %d bytes (epistula-api annotation contract)", apiMaxModelBytes)
	}
	if strings.ContainsRune(c.Worker.AnnotationModel, '\x00') {
		return errors.New("worker.annotation_model must not contain NUL")
	}
	// The configured vocabularies must themselves be producible under the
	// epistula-api contract, or a category or tag this worker is told to emit is
	// rejected 422 on every message that gets it (RA6X-044). Checked on the
	// NORMALIZED form, because that is what is actually sent.
	for _, cat := range c.Worker.Categories {
		if n := normalizeLabel(cat); n == "" {
			return fmt.Errorf("worker.categories contains %q, which normalizes to nothing", cat)
		} else if len(n) > apiMaxCategoryBytes {
			return fmt.Errorf("worker.categories entry %q exceeds %d bytes normalized (epistula-api annotation contract)", cat, apiMaxCategoryBytes)
		}
	}
	for _, tag := range c.Worker.AllowedTags {
		if n := normalizeLabel(tag); n == "" {
			return fmt.Errorf("worker.allowed_tags contains %q, which normalizes to nothing", tag)
		} else if len(n) > apiMaxTagBytes {
			return fmt.Errorf("worker.allowed_tags entry %q exceeds %d bytes normalized (epistula-api annotation contract)", tag, apiMaxTagBytes)
		}
	}
	// A configured category vocabulary must contain the fallback the
	// normalizer will actually use; fallbackCategory picks one from the
	// configured set, so the only unusable configuration is an empty one
	// combined with an allowed-tags list that admits nothing.
	if len(c.Worker.Categories) == 0 {
		return errors.New("worker.categories must not be empty")
	}
	if c.Classify.MaxBodyChars <= 0 {
		return errors.New("classify.max_body_chars must be positive")
	}
	if c.Worker.RetryAttempts < 0 {
		return errors.New("worker.retry_attempts must be non-negative")
	}
	if d, err := time.ParseDuration(c.Worker.RetryBackoff); err != nil || d < 0 {
		return fmt.Errorf("worker.retry_backoff must be a non-negative duration: %q", c.Worker.RetryBackoff)
	}
	// Cross-contract check (RO5X-015), in the spirit of the R-033 checks
	// above: the export stream's idle watchdog is stopped across the
	// per-message LLM work, so this is no longer a correctness requirement —
	// but a worst-case per-message budget far larger than the epistula-api
	// request timeout still means a stalled LM Studio holds an export
	// connection open for a very long time. Surface it at check-config
	// rather than mid-pass.
	//
	// A warning, not an error: the combination is legitimate on a slow model
	// and an operator may knowingly accept it.
	if c.MailAPI.RequestTimeoutSec > 0 && c.LMStudio.RequestTimeoutSec > 0 {
		backoff, _ := time.ParseDuration(c.Worker.RetryBackoff)
		worstCase := time.Duration(c.LMStudio.RequestTimeoutSec)*time.Second*time.Duration(c.Worker.RetryAttempts+1) +
			backoff*time.Duration(c.Worker.RetryAttempts)
		apiTimeout := time.Duration(c.MailAPI.RequestTimeoutSec) * time.Second
		if worstCase > apiTimeout {
			slog.Warn("per-message worst case exceeds the epistula-api request timeout; "+
				"a stalled model will hold an export connection open",
				"worst_case", worstCase,
				"mail_api_request_timeout", apiTimeout,
				"lmstudio_request_timeout", time.Duration(c.LMStudio.RequestTimeoutSec)*time.Second,
				"retry_attempts", c.Worker.RetryAttempts,
				"retry_backoff", backoff)
		}
	}
	if c.Admin.ListenAddr == "" {
		return errors.New("admin.listen_addr is required")
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid logging.level %q", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("invalid logging.format %q", c.Logging.Format)
	}
	if c.Production {
		if err := c.validateProduction(); err != nil {
			return fmt.Errorf("production strict-check: %w", err)
		}
	}
	return nil
}

func (c *Config) validateProduction() error {
	apiURL, _ := parseBaseURL(c.MailAPI.BaseURL)
	if apiURL.Scheme != "https" && !isLoopbackHost(apiURL.Hostname()) {
		return errors.New("mail_api.base_url must be https unless it is loopback")
	}
	lmURL, _ := parseBaseURL(c.LMStudio.BaseURL)
	if !isLoopbackHost(lmURL.Hostname()) {
		// Off-box LM Studio: the bearer token AND the full message text both
		// transit the LAN. Require TLS as well as a token — matching the
		// mail_api side (R-058). LM Studio has no native TLS, so in practice
		// this means fronting it with a tunnel/reverse proxy that terminates
		// https and pointing base_url at that endpoint (see CLAUDE.md).
		if lmURL.Scheme != "https" {
			return errors.New("lmstudio.base_url must be https unless it is loopback (front off-box LM Studio with a TLS tunnel/proxy)")
		}
		if strings.TrimSpace(c.LMStudio.APIToken) == "" {
			return errors.New("lmstudio.api_token is required when LM Studio is not loopback")
		}
	}
	if !isLoopbackListen(c.Admin.ListenAddr) {
		return errors.New("admin.listen_addr must be loopback in production")
	}
	return nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("must use http or https")
	}
	if u.Host == "" {
		return nil, errors.New("host is required")
	}
	return u, nil
}

// isLoopbackListen / isLoopbackHost are a LOCAL COPY of the canonical
// implementation in epistula-database/netutil (RO5X-030).
//
// This project deliberately does not depend on epistula-database — like epistula-mcp,
// it is a pure consumer of epistula-api's HTTP contract, with no schema import and
// no `replace` directive — so importing that package just for two predicates
// would couple a project the architecture keeps decoupled. Instead the
// behaviour is pinned to netutil's exported truth table by
// loopback_ro5x030_test.go, so a divergence fails the build rather than
// lurking.
//
// If you change these, change epistula-database/netutil first and copy it here.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Fail closed: a bare ":8790" or a malformed value must never be
		// mistaken for a safe bind.
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	// EqualFold: DNS names are case-insensitive, so LOCALHOST is loopback.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) RetryBackoffDuration() time.Duration {
	d, _ := time.ParseDuration(c.Worker.RetryBackoff)
	return d
}
