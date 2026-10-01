// Configuration lives in TOML, never a .env file.
//
// Precedence (highest first):
//  1. Environment (MAIL_MCP_BASE_URL / MAIL_MCP_TOKEN / MAIL_MCP_ANNOTATE_TOKEN /
//     MAIL_MCP_HTTP_TOKEN) — the MCP client injects these when it launches the
//     stdio connector, so the stdio path can run with ZERO files and no secret
//     sits in a file on the client's workstation.
//  2. The TOML file (-config, or the default search path if a file exists there).
//  3. Built-in defaults (request_timeout=30s, max_limit=200, max_text_bytes=64KiB).
//
// Three secrets live here and must never leak: the epistula-api read `token`, the
// optional `annotate_token`, and the `http_token` gating the -http transport.
// None is ever echoed in an error string, a log line, or a URL (see the
// config-load errors below and client.go).
package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	toml "github.com/pelletier/go-toml/v2"
)

// Default search path, in order. The first existing file wins when -config is not
// given. Matches the Epistula convention (/usr/local/etc/epistula first).
var configSearchPath = []string{
	"/usr/local/etc/epistula/epistula-mcp.toml",
	"/etc/epistula/epistula-mcp.toml",
	"./epistula-mcp.toml",
}

// defaultConfigPath is only used in the -help text for -config.
const defaultConfigPath = "/usr/local/etc/epistula/epistula-mcp.toml"

const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxLimit       = 200
	defaultMaxTextBytes   = 64 * 1024 // 65536

	// defaultMaxResponseBytes bounds how much of an upstream response the
	// connector will buffer. client.do used io.ReadAll with no cap, so a
	// single message_text call on a large message allocated the whole body
	// here — and the MCP typically runs as a subprocess of a desktop client
	// on a laptop, where a 50 MiB spike is user-visible. The package doc
	// already claimed this discipline; maxErrBody only ever bounded the
	// *error* path, after the full body was resident (RO5X-016).
	defaultMaxResponseBytes = 8 << 20 // 8 MiB, well above any slimmed JSON page

	// minMaxResponseBytes floors the cap so a small max_text_bytes cannot
	// make ordinary list pages start failing.
	minMaxResponseBytes = 4 << 20
)

// fileConfig is the on-disk TOML shape:
//
//	[mailapi]
//	base_url        = "http://127.0.0.1:8784"
//	token           = ""
//	request_timeout = "30s"
//
//	[mcp]
//	http_token     = ""
//	max_limit      = 200
//	max_text_bytes = 65536
//	max_response_bytes = 8388608
type fileConfig struct {
	MailAPI struct {
		BaseURL        string `toml:"base_url"`
		Token          string `toml:"token"`
		AnnotateToken  string `toml:"annotate_token"`
		RequestTimeout string `toml:"request_timeout"`
	} `toml:"mailapi"`
	MCP struct {
		HTTPToken        string `toml:"http_token"`
		MaxLimit         int    `toml:"max_limit"`
		MaxTextBytes     int    `toml:"max_text_bytes"`
		MaxResponseBytes int    `toml:"max_response_bytes"`
	} `toml:"mcp"`
}

// config is the resolved, validated configuration the rest of the program uses.
type config struct {
	BaseURL string // epistula-api root, no trailing slash, e.g. http://127.0.0.1:8784
	Token   string // epistula-api bearer token used by the READ tools (secret)
	// AnnotateToken is a SEPARATE epistula-api token used only by the `annotate`
	// tool. Empty is the default and the safe posture: with no annotate
	// credential the tool is not registered at all, so a session that reads
	// hostile message content has no write capability to reach.
	//
	// Why two tokens rather than one carrying write_annotation: email is
	// attacker-controlled input, so any message body can carry text aimed at
	// the model reading it. Splitting the credential means the token exposed
	// to that content cannot write, enforced upstream at the SQL level rather
	// than by this proxy's good behaviour.
	AnnotateToken  string        // secret; empty ⇒ no annotate tool
	RequestTimeout time.Duration // per-request total deadline
	HTTPToken      string        // bearer gating the -http transport (secret)
	MaxLimit       int           // clamp for every list/search limit
	MaxTextBytes   int           // cap on message text returned to the model
	// MaxResponseBytes bounds a buffered upstream response body.
	MaxResponseBytes int
}

// loadConfig reads the TOML file (explicit -config, else the first existing file
// in the search path), applies env overrides, and validates. It never places a
// secret in a returned error.
func loadConfig(path string) (config, error) {
	var fc fileConfig

	if path == "" {
		for _, p := range configSearchPath {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := toml.Unmarshal(raw, &fc); err != nil {
			return config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// Env overrides win over the file so the stdio path can run with zero files.
	if v := strings.TrimSpace(os.Getenv("MAIL_MCP_BASE_URL")); v != "" {
		fc.MailAPI.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("MAIL_MCP_TOKEN")); v != "" {
		fc.MailAPI.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("MAIL_MCP_ANNOTATE_TOKEN")); v != "" {
		fc.MailAPI.AnnotateToken = v
	}
	if v := strings.TrimSpace(os.Getenv("MAIL_MCP_HTTP_TOKEN")); v != "" {
		fc.MCP.HTTPToken = v
	}

	cfg := config{
		BaseURL:          strings.TrimRight(strings.TrimSpace(fc.MailAPI.BaseURL), "/"),
		Token:            strings.TrimSpace(fc.MailAPI.Token),
		AnnotateToken:    strings.TrimSpace(fc.MailAPI.AnnotateToken),
		HTTPToken:        strings.TrimSpace(fc.MCP.HTTPToken),
		RequestTimeout:   defaultRequestTimeout,
		MaxLimit:         defaultMaxLimit,
		MaxTextBytes:     defaultMaxTextBytes,
		MaxResponseBytes: defaultMaxResponseBytes,
	}

	if cfg.Token == "" {
		return config{}, fmt.Errorf("no epistula-api token: set [mailapi] token in the TOML config " +
			"or MAIL_MCP_TOKEN (mint one with `epistula-database admin api-token-add`)")
	}

	// Reject an annotate_token identical to token. A config that names two
	// credentials but supplies one is worse than not splitting at all: it reads
	// as a read/write separation while the read tools still hold write scope,
	// so the assurance is false. Fail loud rather than pretend.
	// Compared with subtle.ConstantTimeCompare out of habit — these are secrets
	// and there is no reason to leak a length-or-prefix timing signal even here.
	if cfg.AnnotateToken != "" && len(cfg.AnnotateToken) == len(cfg.Token) &&
		subtle.ConstantTimeCompare([]byte(cfg.AnnotateToken), []byte(cfg.Token)) == 1 {
		return config{}, fmt.Errorf("annotate_token is identical to token: the point of the " +
			"second credential is that the read tools cannot write, so mint a read-only token " +
			"(read_metadata + read_content) for `token` and a separate write_annotation token " +
			"for `annotate_token` — or leave annotate_token empty to drop the annotate tool")
	}

	if cfg.BaseURL == "" {
		return config{}, fmt.Errorf("no epistula-api base_url: set [mailapi] base_url in the TOML config " +
			"or MAIL_MCP_BASE_URL (e.g. http://127.0.0.1:8784 or https://epistula-api.example.invalid)")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		// The URL may embed userinfo; report only the scheme/host shape, never the raw value.
		return config{}, fmt.Errorf("base_url must be an http or https URL with a host")
	}
	// https-only except loopback: the bearer token rides in a request header, so
	// never let a non-loopback base_url downgrade it to cleartext.
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return config{}, fmt.Errorf("base_url must be https for non-loopback host %q "+
			"(the epistula-api token must never ride a cleartext non-loopback request)", u.Hostname())
	}

	if s := strings.TrimSpace(fc.MailAPI.RequestTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return config{}, fmt.Errorf("request_timeout: %w", err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("request_timeout must be positive, got %q", s)
		}
		cfg.RequestTimeout = d
	}
	if fc.MCP.MaxLimit > 0 {
		cfg.MaxLimit = fc.MCP.MaxLimit
	}
	if fc.MCP.MaxResponseBytes > 0 {
		cfg.MaxResponseBytes = fc.MCP.MaxResponseBytes
	}
	if fc.MCP.MaxTextBytes > 0 {
		// A budget narrower than the widest UTF-8 rune cannot hold one
		// character, and a page that cannot hold a character cannot advance
		// (RA6X-018): capText backed off past the cut, returned nothing, and
		// reported truncated=true, so next_offset equalled the input offset
		// forever and a model paging a body looped. Refusing the configuration
		// is better than silently overshooting it on every page.
		if fc.MCP.MaxTextBytes < utf8.UTFMax {
			return config{}, fmt.Errorf("max_text_bytes must be at least %d (one UTF-8 rune), got %d",
				utf8.UTFMax, fc.MCP.MaxTextBytes)
		}
		cfg.MaxTextBytes = fc.MCP.MaxTextBytes
	}

	// Size the response cap against max_text_bytes so raising the text
	// budget does not silently start failing /text fetches: the JSON
	// envelope plus escaping can several-times inflate a text body.
	if want := cfg.MaxTextBytes * 8; want > cfg.MaxResponseBytes {
		cfg.MaxResponseBytes = want
	}
	if cfg.MaxResponseBytes < minMaxResponseBytes {
		cfg.MaxResponseBytes = minMaxResponseBytes
	}
	return cfg, nil
}

// isLoopbackHost reports whether a URL hostname refers to loopback. "localhost"
// and any IP that parses as loopback (127.0.0.0/8, ::1) count; everything else
// (including an empty host) does not.
//
// It is epistula-mcp's single LOCAL COPY of the canonical implementation in
// epistula-database/netutil (RO5X-030).
//
// This project cannot import that package by design — it depends only on
// epistula-api's /v1 HTTP contract, with no schema import and no `replace`
// directive, so a migration never triggers a lockstep rebuild here. The
// behaviour is instead pinned to netutil's truth table by
// loopback_ro5x030_test.go.
//
// If you change this, change epistula-database/netutil first and copy it here.
func isLoopbackHost(host string) bool {
	// EqualFold, not ==: DNS names are case-insensitive, so LOCALHOST is
	// loopback. The old == comparison was the divergence RO5X-030 found.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
