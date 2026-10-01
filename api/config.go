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
	"/usr/local/etc/epistula/epistula-api.toml",
	"/etc/epistula/epistula-api.toml",
	"epistula-api.toml",
}

type Config struct {
	Production bool           `toml:"production"`
	Server     ServerConfig   `toml:"server"`
	Admin      AdminConfig    `toml:"admin"`
	Postgres   PostgresConfig `toml:"postgres"`
	Storage    StorageConfig  `toml:"storage"`
	Limits     LimitsConfig   `toml:"limits"`
	Logging    LoggingConfig  `toml:"logging"`
}

// ServerConfig is the JSON API listener. Plain HTTP on loopback; Apache
// terminates TLS (and optional mTLS) in front.
type ServerConfig struct {
	ListenAddr         string `toml:"listen_addr"`
	ReadTimeoutSec     int    `toml:"read_timeout_seconds"`
	WriteTimeoutSec    int    `toml:"write_timeout_seconds"` // 0 = unlimited (export streams are long-lived)
	IdleTimeoutSec     int    `toml:"idle_timeout_seconds"`
	ShutdownTimeoutSec int    `toml:"shutdown_timeout_seconds"`
}

// AdminConfig is the loopback metrics/health listener.
type AdminConfig struct {
	ListenAddr         string `toml:"listen_addr"`
	ExposeMetrics      bool   `toml:"expose_metrics"`
	ReadTimeoutSec     int    `toml:"read_timeout_seconds"`
	WriteTimeoutSec    int    `toml:"write_timeout_seconds"`
	ShutdownTimeoutSec int    `toml:"shutdown_timeout_seconds"`
}

type PostgresConfig struct {
	DSN              string `toml:"dsn"`
	StatementTimeout string `toml:"statement_timeout"`
	MaxOpenConns     int    `toml:"max_open_conns"`
	// MaxIdleConns maps onto pgxpool MinConns — the number of connections
	// kept warm — for key-name consistency with epistula-database.
	MaxIdleConns    int    `toml:"max_idle_conns"`
	ConnMaxLifetime string `toml:"conn_max_lifetime"`
}

type StorageConfig struct {
	Root string `toml:"root"`
}

type LimitsConfig struct {
	// DefaultPageSize / MaxPageSize bound cursor-paginated list responses.
	DefaultPageSize int `toml:"default_page_size"`
	MaxPageSize     int `toml:"max_page_size"`
	// MaxExportStreams caps concurrent /v1/export streams server-wide.
	MaxExportStreams int `toml:"max_export_streams"`
	// ExportPageSize is the internal batch size the export stream queries
	// with; each batch is one bounded statement, so statement_timeout
	// applies per batch, not to the whole corpus walk.
	ExportPageSize int `toml:"export_page_size"`
	// ExportWriteStall bounds how long ONE write to an export stream may make
	// no progress before the stream is torn down (RA6X-063).
	//
	// A connected, authenticated client that stops reading blocks the handler
	// inside Write once the socket buffers fill, and the export semaphore slot
	// is only released when the handler returns — so two stalled readers
	// exhaust the default capacity and every other worker gets 429. The
	// server's WriteTimeout cannot be used for this: it is a deadline on the
	// WHOLE response, and an export is legitimately long-lived. IdleTimeout
	// does not apply either — it bounds an idle keep-alive connection, not an
	// active response write.
	//
	// This is a ROLLING deadline, refreshed for every 8 KiB write/flush,
	// including inside a large row, so an export that keeps making progress runs as long as it
	// needs to while one that stops making progress dies promptly. It is
	// enforced by this process on every connection path, rather than delegated
	// to a reverse proxy that may or may not be in front of a given client.
	// The default is coordinated with epistula-llm-worker's worst case for one
	// message, which is why it is measured in tens of minutes rather than
	// seconds: the worker legitimately stops reading while a model runs.
	// A duration string; empty selects 30m; "0s" disables (do not disable
	// unless something else enforces client liveness).
	ExportWriteStall string `toml:"export_write_stall"`
	// MaxPageBytes bounds the AGGREGATE retained bytes of one scanned page —
	// export batches and text-enabled list pages alike (RA6X-040).
	//
	// Page size alone bounds rows, not memory, and a message may be 50 MiB, so
	// a 500-row page could ask for tens of gigabytes. A page that reaches this
	// budget is cut short and resumes from its own cursor, so completeness and
	// ordering are unaffected; the first row of a page is always admitted
	// however large, so a single huge message can still be exported.
	// 0 selects 32 MiB.
	MaxPageBytes int `toml:"max_page_bytes"`
	// AuthFailLimit / AuthFailWindow throttle bad bearer tokens per IP.
	AuthFailLimit  int    `toml:"auth_fail_limit"`
	AuthFailWindow string `toml:"auth_fail_window"`
	// AuthFailDelay is the fixed response delay on a failed verification,
	// blunting token-guessing without a measurable verify-time oracle.
	AuthFailDelay string `toml:"auth_fail_delay"`
	// AuthCacheTTL bounds the in-memory verified-token cache. A revoked
	// token keeps working at most this long.
	AuthCacheTTL string `toml:"auth_cache_ttl"`
	// MaxAnnotationBytes bounds the PUT /v1/messages/{id}/annotation body.
	MaxAnnotationBytes int64 `toml:"max_annotation_bytes"`
	// PerTokenRate / PerTokenBurst configure the per-token request rate
	// limiter (token bucket, keyed by token id). Rate is requests/second;
	// 0 (the default) disables it. Set well above a worker's steady rate so
	// legitimate throughput is unaffected — it exists to blunt a compromised
	// or runaway token, not to shape normal traffic. (R-017)
	PerTokenRate  float64 `toml:"per_token_rate"`
	PerTokenBurst int     `toml:"per_token_burst"`
}

type LoggingConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

func DefaultConfig() *Config {
	return &Config{
		Production: false,
		Server: ServerConfig{
			ListenAddr:         "127.0.0.1:8784",
			ReadTimeoutSec:     30,
			WriteTimeoutSec:    0,
			IdleTimeoutSec:     120,
			ShutdownTimeoutSec: 30,
		},
		Admin: AdminConfig{
			ListenAddr:         "127.0.0.1:8785",
			ExposeMetrics:      true,
			ReadTimeoutSec:     5,
			WriteTimeoutSec:    10,
			ShutdownTimeoutSec: 30,
		},
		Postgres: PostgresConfig{
			StatementTimeout: "10s",
			MaxOpenConns:     20,
			MaxIdleConns:     5,
			ConnMaxLifetime:  "5m",
		},
		Storage: StorageConfig{
			Root: "/var/spool/epistula-database",
		},
		Limits: LimitsConfig{
			DefaultPageSize:    50,
			MaxPageSize:        500,
			MaxExportStreams:   2,
			ExportPageSize:     500,
			ExportWriteStall:   "30m",
			MaxPageBytes:       defaultMaxPageBytes,
			AuthFailLimit:      10,
			AuthFailWindow:     "60s",
			AuthFailDelay:      "250ms",
			AuthCacheTTL:       "60s",
			MaxAnnotationBytes: 256 * 1024,
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

	// Environment variables OVERRIDE the file, and are applied whether or not
	// a config file was found.
	//
	// This used to be an else-branch: env was a *fallback* consulted only when
	// no TOML existed, so an operator who set MAIL_API_* alongside a config file saw
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
	if v := os.Getenv("MAIL_API_DSN"); v != "" {
		cfg.Postgres.DSN = v
	}
	if v := os.Getenv("MAIL_API_STORAGE_ROOT"); v != "" {
		cfg.Storage.Root = v
	}
	if v := os.Getenv("MAIL_API_LISTEN_ADDR"); v != "" {
		cfg.Server.ListenAddr = v
	}
	if v := os.Getenv("MAIL_API_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("MAIL_API_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv("MAIL_API_PRODUCTION"); v == "true" || v == "1" {
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
	if c.Storage.Root == "" {
		return errors.New("storage.root is required")
	}
	if c.Server.ListenAddr == "" {
		return errors.New("server.listen_addr is required")
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
	if c.Limits.MaxPageBytes < 0 {
		return fmt.Errorf("limits.max_page_bytes must not be negative: %d", c.Limits.MaxPageBytes)
	}
	if c.Limits.DefaultPageSize <= 0 {
		return errors.New("limits.default_page_size must be positive")
	}
	if c.Limits.MaxPageSize < c.Limits.DefaultPageSize {
		return fmt.Errorf("limits.max_page_size (%d) must be >= limits.default_page_size (%d)",
			c.Limits.MaxPageSize, c.Limits.DefaultPageSize)
	}
	if c.Limits.MaxExportStreams <= 0 {
		return errors.New("limits.max_export_streams must be positive")
	}
	if c.Limits.ExportPageSize <= 0 {
		return errors.New("limits.export_page_size must be positive")
	}
	if c.Limits.ExportWriteStall != "" {
		if d, err := time.ParseDuration(c.Limits.ExportWriteStall); err != nil || d < 0 {
			return fmt.Errorf("limits.export_write_stall must be a non-negative duration: %q", c.Limits.ExportWriteStall)
		}
	}
	if c.Limits.PerTokenRate < 0 {
		return errors.New("limits.per_token_rate must be non-negative (0 disables)")
	}
	if c.Limits.PerTokenRate > 0 && c.Limits.PerTokenBurst < 1 {
		return errors.New("limits.per_token_burst must be >= 1 when per_token_rate is set")
	}
	if c.Limits.AuthFailLimit <= 0 {
		return errors.New("limits.auth_fail_limit must be positive")
	}
	if d, err := time.ParseDuration(c.Limits.AuthFailWindow); err != nil || d <= 0 {
		return fmt.Errorf("limits.auth_fail_window must be a positive duration: %q", c.Limits.AuthFailWindow)
	}
	if d, err := time.ParseDuration(c.Limits.AuthFailDelay); err != nil || d < 0 {
		return fmt.Errorf("limits.auth_fail_delay must be a non-negative duration: %q", c.Limits.AuthFailDelay)
	}
	if d, err := time.ParseDuration(c.Limits.AuthCacheTTL); err != nil || d < 0 {
		return fmt.Errorf("limits.auth_cache_ttl must be a non-negative duration: %q", c.Limits.AuthCacheTTL)
	}
	if c.Limits.MaxAnnotationBytes <= 0 {
		return errors.New("limits.max_annotation_bytes must be positive")
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
	if !isLoopbackAddr(c.Server.ListenAddr) {
		return fmt.Errorf("server.listen_addr %q must bind loopback — Apache terminates TLS in front; the Go process never faces the network", c.Server.ListenAddr)
	}
	if !isLoopbackAddr(c.Admin.ListenAddr) {
		return fmt.Errorf("admin.listen_addr %q must bind loopback — the metrics listener is unauthenticated and must never be exposed beyond the host", c.Admin.ListenAddr)
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

func (c *Config) ConnMaxLifetimeDuration() time.Duration {
	d, _ := time.ParseDuration(c.Postgres.ConnMaxLifetime)
	return d
}

func (c *Config) AuthFailWindowDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.AuthFailWindow)
	return d
}

func (c *Config) AuthFailDelayDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.AuthFailDelay)
	return d
}

func (c *Config) AuthCacheTTLDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.AuthCacheTTL)
	return d
}
