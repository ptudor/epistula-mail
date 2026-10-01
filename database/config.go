package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/ptudor/epistula-mail/database/netutil"
	"github.com/ptudor/epistula-mail/database/pgdsn"
)

var defaultConfigPaths = []string{
	"/usr/local/etc/epistula/epistula-database.toml",
	"/etc/epistula/epistula-database.toml",
	"epistula-database.toml",
}

type Config struct {
	Production bool           `toml:"production"`
	Postgres   PostgresConfig `toml:"postgres"`
	Storage    StorageConfig  `toml:"storage"`
	Delivery   DeliveryConfig `toml:"delivery"`
	Limits     LimitsConfig   `toml:"limits"`
	Logging    LoggingConfig  `toml:"logging"`
	HTTP       HTTPConfig     `toml:"http"`
	Argon2     Argon2Config   `toml:"argon2"`
	Admin      AdminConfig    `toml:"admin"`
}

type PostgresConfig struct {
	DSN              string `toml:"dsn"`
	StatementTimeout string `toml:"statement_timeout"`
	MaxOpenConns     int    `toml:"max_open_conns"`
	// MaxIdleConns maps onto pgxpool's MinConns. The names differ because
	// pgx has no max-idle concept: MinConns is the number of connections the
	// pool keeps warm, which is the closest equivalent. The sibling configs
	// (epistula-imap, epistula-api) document the same mismatch (RO5X-021).
	MaxIdleConns    int    `toml:"max_idle_conns"`
	ConnMaxLifetime string `toml:"conn_max_lifetime"`
}

type StorageConfig struct {
	Root string `toml:"root"`
	// GroupWritable enables shared-group blob permissions (setgid dirs 2770,
	// files 0660) so a cooperating daemon in the storage group — epistula-imap
	// doing IMAP APPEND — can write into the tree the LDA creates. Leave false
	// for a single-daemon deployment (owner-only 0750/0640). See R-007.
	GroupWritable bool `toml:"group_writable"`
}

type DeliveryConfig struct {
	Timeout       string `toml:"timeout"`
	DefaultFolder string `toml:"default_folder"`

	// PostHookCommand, if non-empty, is exec'd after a successful Ingest +
	// commit. The hook runs synchronously with its own timeout (so a wedged
	// hook cannot indefinitely stall a Postfix pipe worker) but its exit
	// status is logged, not propagated — the message is already durable in
	// PG by the time the hook fires. Env vars passed to the hook:
	//
	//   MAIL_DB_ENVELOPE_FROM   (verbatim)
	//   MAIL_DB_ENVELOPE_TO     (verbatim)
	//   MAIL_DB_MAILBOX_ID
	//   MAIL_DB_FOLDER_ID
	//   MAIL_DB_UID
	//   MAIL_DB_MESSAGE_ID
	//   MAIL_DB_MODSEQ
	//   MAIL_DB_RAW_SHA256      (full 64-char hex)
	//   MAIL_DB_RAW_BYTES
	//   MAIL_DB_ATTACHMENTS     (count)
	//   MAIL_DB_IS_CATCHALL     ("true" | "false")
	//
	// The hook receives no stdin and its stdout/stderr are inherited so
	// hook diagnostics show up in the mail log. Per CLAUDE.md spam
	// filtering belongs upstream of the LDA; this hook is intended for
	// archive indexing, statistics, or operator-defined notifications.
	PostHookCommand []string `toml:"post_hook_command"`
	PostHookTimeout string   `toml:"post_hook_timeout"`
}

type LimitsConfig struct {
	MaxMessageBytes       int64 `toml:"max_message_bytes"`
	MaxMimeDepth          int   `toml:"max_mime_depth"`
	MaxMimeParts          int   `toml:"max_mime_parts"`
	MaxHeaderBytes        int   `toml:"max_header_bytes"`
	MaxHeaderSectionBytes int   `toml:"max_header_section_bytes"`
	MaxTransferExpansion  int   `toml:"max_transfer_expansion"`
}

type LoggingConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

type HTTPConfig struct {
	ListenAddr         string `toml:"listen_addr"`
	ReadTimeoutSec     int    `toml:"read_timeout_seconds"`
	WriteTimeoutSec    int    `toml:"write_timeout_seconds"`
	ShutdownTimeoutSec int    `toml:"shutdown_timeout_seconds"`
	ExposeMetrics      bool   `toml:"expose_metrics"`
}

type Argon2Config struct {
	Memory     uint32 `toml:"memory_kib"`
	Iterations uint32 `toml:"iterations"`
	Parallel   uint8  `toml:"parallelism"`
	KeyLen     uint32 `toml:"key_len"`
	SaltLen    uint32 `toml:"salt_len"`
}

// AdminConfig holds operator-CLI policy. Kept small on purpose: dashboard or
// web-administration settings do NOT belong here.
type AdminConfig struct {
	// MinPasswordLength is enforced by `mailbox-add` / `mailbox-passwd` for
	// both interactive and -password-stdin input. Argon2id is resistant to
	// brute force but a 4-character password is still a 4-character
	// password; the floor defaults to 12. Bounded below by 8.
	MinPasswordLength int `toml:"min_password_length"`
	// MaxPasswordLength bounds Argon2 input size so a multi-MB stdin
	// blob cannot wedge the hasher. 1 KiB is far above any plausible
	// human password and well below the point where Argon2id slows down.
	MaxPasswordLength int `toml:"max_password_length"`
}

func DefaultConfig() *Config {
	return &Config{
		Production: false,
		Postgres: PostgresConfig{
			StatementTimeout: "30s",
			MaxOpenConns:     20,
			MaxIdleConns:     5,
			ConnMaxLifetime:  "1h",
		},
		Storage: StorageConfig{
			Root: "/var/spool/epistula-database",
		},
		Delivery: DeliveryConfig{
			Timeout:         "60s",
			DefaultFolder:   "INBOX",
			PostHookCommand: nil,
			PostHookTimeout: "10s",
		},
		Limits: LimitsConfig{
			MaxMessageBytes:       52_428_800,
			MaxMimeDepth:          10,
			MaxMimeParts:          200,
			MaxHeaderBytes:        16 * 1024,
			MaxHeaderSectionBytes: 256 * 1024,
			MaxTransferExpansion:  10,
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
		HTTP: HTTPConfig{
			ListenAddr:         "127.0.0.1:8782",
			ReadTimeoutSec:     5,
			WriteTimeoutSec:    10,
			ShutdownTimeoutSec: 30,
			ExposeMetrics:      true,
		},
		Argon2: Argon2Config{
			Memory:     64 * 1024,
			Iterations: 3,
			Parallel:   4,
			KeyLen:     32,
			SaltLen:    16,
		},
		Admin: AdminConfig{
			MinPasswordLength: 12,
			MaxPasswordLength: 1024,
		},
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

	// Environment variables OVERRIDE the file, and are applied whether or not
	// a config file was found.
	//
	// This used to be an else-branch: env was a *fallback* consulted only when
	// no TOML existed, so an operator who set MAIL_DATABASE_* alongside a config file saw
	// it silently ignored — while the same operator's setting in llm-worker or
	// epistula-mcp (which always override) would win. Three daemons treating env
	// as fallback and two as override is a live hazard in a deployment that
	// mixes both, and especially for secrets, which is exactly the case env
	// exists to serve (RO5X-031).
	//
	// Precedence is now uniform across all five projects:
	//     env > config file > built-in defaults
	applyEnv(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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
	if v := os.Getenv("MAIL_DATABASE_DSN"); v != "" {
		cfg.Postgres.DSN = v
	}
	if v := os.Getenv("MAIL_DATABASE_STORAGE_ROOT"); v != "" {
		cfg.Storage.Root = v
	}
	if v := os.Getenv("MAIL_DATABASE_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("MAIL_DATABASE_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv("MAIL_DATABASE_PRODUCTION"); v == "true" || v == "1" {
		cfg.Production = true
	}
}

func (c *Config) Validate() error {
	if c.Postgres.DSN == "" {
		return errors.New("postgres.dsn is required")
	}
	if _, err := time.ParseDuration(c.Postgres.StatementTimeout); err != nil {
		return fmt.Errorf("postgres.statement_timeout: %w", err)
	}
	if _, err := time.ParseDuration(c.Postgres.ConnMaxLifetime); err != nil {
		return fmt.Errorf("postgres.conn_max_lifetime: %w", err)
	}
	if d, err := time.ParseDuration(c.Delivery.Timeout); err != nil {
		return fmt.Errorf("delivery.timeout: %w", err)
	} else if d <= 0 {
		// A zero/negative timeout makes time.AfterFunc fire immediately, so
		// the watchdog kills every delivery with EX_TEMPFAIL — a total outage
		// (R-030).
		return errors.New("delivery.timeout must be positive")
	}
	if len(c.Delivery.PostHookCommand) > 0 {
		hookTimeout := c.Delivery.PostHookTimeout
		if hookTimeout == "" {
			hookTimeout = "10s"
		}
		d, err := time.ParseDuration(hookTimeout)
		if err != nil {
			return fmt.Errorf("delivery.post_hook_timeout: %w", err)
		}
		if d <= 0 {
			return errors.New("delivery.post_hook_timeout must be positive")
		}
		// Refuse a hook command whose first element is not an absolute
		// path. Hooks run with the LDA's PATH, which under Postfix is
		// minimal and surprising — requiring an absolute path makes
		// "command not found" a config error caught at startup rather
		// than per-delivery.
		if !strings.HasPrefix(c.Delivery.PostHookCommand[0], "/") {
			return errors.New("delivery.post_hook_command[0] must be an absolute path")
		}
	}
	if c.Storage.Root == "" {
		return errors.New("storage.root is required")
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
	if c.Postgres.MaxOpenConns < 1 {
		return errors.New("postgres.max_open_conns must be >= 1")
	}
	if c.Postgres.MaxIdleConns < 0 || c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		return fmt.Errorf("postgres.max_idle_conns (%d) must be between 0 and max_open_conns (%d)",
			c.Postgres.MaxIdleConns, c.Postgres.MaxOpenConns)
	}

	if c.Limits.MaxMessageBytes <= 0 {
		return errors.New("limits.max_message_bytes must be positive")
	}
	if c.Limits.MaxMimeDepth <= 0 {
		return errors.New("limits.max_mime_depth must be positive")
	}
	if c.Limits.MaxMimeParts <= 0 {
		return errors.New("limits.max_mime_parts must be positive")
	}
	if c.Limits.MaxTransferExpansion <= 0 {
		return errors.New("limits.max_transfer_expansion must be positive")
	}
	// A zero/negative header limit makes checkHeaderSection reject EVERY
	// message → EX_DATAERR → all inbound mail bounces (R-030).
	if c.Limits.MaxHeaderBytes <= 0 {
		return errors.New("limits.max_header_bytes must be positive")
	}
	if c.Limits.MaxHeaderSectionBytes <= 0 {
		return errors.New("limits.max_header_section_bytes must be positive")
	}
	if c.Limits.MaxHeaderSectionBytes < c.Limits.MaxHeaderBytes {
		return fmt.Errorf("limits.max_header_section_bytes (%d) must be >= limits.max_header_bytes (%d)",
			c.Limits.MaxHeaderSectionBytes, c.Limits.MaxHeaderBytes)
	}
	if c.Argon2.Memory < 8*1024 {
		return errors.New("argon2.memory_kib must be >= 8192 (8 MiB)")
	}
	if c.Argon2.Iterations < 1 {
		return errors.New("argon2.iterations must be >= 1")
	}
	if c.Argon2.Parallel < 1 {
		return errors.New("argon2.parallelism must be >= 1")
	}
	// Reject a weak salt/key length outright rather than silently patching it
	// (R-030). OWASP floors: 16-byte salt, 32-byte key.
	if c.Argon2.SaltLen < 16 {
		return errors.New("argon2.salt_len must be >= 16")
	}
	if c.Argon2.KeyLen < 32 {
		return errors.New("argon2.key_len must be >= 32")
	}
	if c.Admin.MinPasswordLength < 8 {
		return errors.New("admin.min_password_length must be >= 8")
	}
	if c.Admin.MaxPasswordLength <= c.Admin.MinPasswordLength {
		return fmt.Errorf("admin.max_password_length (%d) must exceed admin.min_password_length (%d)",
			c.Admin.MaxPasswordLength, c.Admin.MinPasswordLength)
	}

	if c.Production {
		if err := c.validateProduction(); err != nil {
			return fmt.Errorf("production strict-check: %w", err)
		}
	}
	return nil
}

func (c *Config) validateProduction() error {
	// Ask pgx what this DSN actually resolves to rather than searching its
	// text: a percent-escaped sslmode, an application_name containing the
	// string "sslmode=verify-full", or a password with "sslmode=allow" in it
	// all defeat substring matching in one direction or the other (RA6X-029).
	if err := pgdsn.RequireVerifiedTLS(c.Postgres.DSN); err != nil {
		return fmt.Errorf("postgres.dsn: %w", err)
	}
	if c.Argon2.Memory < 16*1024 {
		return errors.New("argon2.memory_kib must be >= 16384 (16 MiB) in production")
	}
	if !isLoopbackAddr(c.HTTP.ListenAddr) {
		return fmt.Errorf("http.listen_addr %q must bind loopback (127.0.0.1/[::1]/localhost) — the metrics listener is unauthenticated and must never be exposed beyond the host", c.HTTP.ListenAddr)
	}
	return nil
}

// isLoopbackAddr reports whether a listen address of the form host:port is
// guaranteed to bind a loopback interface. An empty or wildcard host binds
// every interface and is therefore NOT loopback.
// isLoopbackAddr delegates to the canonical implementation in
// epistula-database/netutil so every project answers this security-relevant
// question identically (RO5X-030).
func isLoopbackAddr(addr string) bool { return netutil.IsLoopbackAddr(addr) }

func (c *Config) StatementTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.Postgres.StatementTimeout)
	return d
}

func (c *Config) DeliveryTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.Delivery.Timeout)
	return d
}

// PostHookTimeoutDuration returns the parsed timeout for the post-delivery
// hook. Defaults to 10s if unset or unparseable — Validate() rejects
// malformed values, so falling back at runtime is purely defensive.
func (c *Config) PostHookTimeoutDuration() time.Duration {
	if c.Delivery.PostHookTimeout == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(c.Delivery.PostHookTimeout)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

func (c *Config) ConnMaxLifetimeDuration() time.Duration {
	d, _ := time.ParseDuration(c.Postgres.ConnMaxLifetime)
	return d
}
