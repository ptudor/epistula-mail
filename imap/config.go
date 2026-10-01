package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/ptudor/epistula-mail/database/netutil"
	"github.com/ptudor/epistula-mail/database/pgdsn"
)

var defaultConfigPaths = []string{
	"/usr/local/etc/epistula/epistula-imap.toml",
	"/etc/epistula/epistula-imap.toml",
	"epistula-imap.toml",
}

type Config struct {
	Production bool           `toml:"production"`
	Server     ServerConfig   `toml:"server"`
	Admin      AdminConfig    `toml:"admin"`
	Postgres   PostgresConfig `toml:"postgres"`
	Storage    StorageConfig  `toml:"storage"`
	Limits     LimitsConfig   `toml:"limits"`
	Archive    ArchiveConfig  `toml:"archive"`
	Logging    LoggingConfig  `toml:"logging"`
}

type ServerConfig struct {
	ListenAddr     string `toml:"listen_addr"`
	TLSCert        string `toml:"tls_cert"`
	TLSKey         string `toml:"tls_key"`
	MinTLSVersion  string `toml:"min_tls_version"`
	AcceptInsecure bool   `toml:"accept_insecure_for_dev"`
	// DrainTimeoutSec bounds the graceful shutdown: after SIGTERM the
	// listener stops accepting, live sessions get this many seconds to
	// finish, then remaining connections are force-closed.
	DrainTimeoutSec int `toml:"drain_timeout_seconds"`
}

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
	// kept warm — because pgx has no max-idle concept. The key name is
	// retained for consistency with the other Epistula daemons' configs.
	MaxIdleConns    int    `toml:"max_idle_conns"`
	ConnMaxLifetime string `toml:"conn_max_lifetime"`
	IdleConns       int    `toml:"idle_conn_pool_size"`
}

type StorageConfig struct {
	Root string `toml:"root"`
	// GroupWritable must match epistula-database's setting: the IMAP daemon writes
	// blobs on APPEND, so a shared-group deployment sets this true so the
	// setgid group-writable modes are honored. See R-007.
	GroupWritable bool `toml:"group_writable"`
}

type LimitsConfig struct {
	PerIPConnections      int    `toml:"per_ip_connections"`
	PerMailboxConnections int    `toml:"per_mailbox_connections"`
	LoginAttempts         int    `toml:"login_attempts"`
	LoginWindow           string `toml:"login_window"`
	LoginCooldown         string `toml:"login_cooldown"`
	CommandRatePerSec     int    `toml:"command_rate_per_sec"`
	IdleTimeout           string `toml:"idle_timeout"`
	MaxAppendBytes        int64  `toml:"max_append_bytes"`
	// MaxSearchResults caps how many rows a single SEARCH may return.
	// SEARCH materializes its whole result set (and the NumSet built from
	// it) in memory, and one command is enough to do it — the command-rate
	// cap does not help. 0 disables the limit.
	MaxSearchResults int `toml:"max_search_results"`
	// TLSHandshakeTimeout bounds how long an accepted connection may take
	// to complete its TLS handshake before being dropped (slowloris
	// defense at the handshake layer).
	TLSHandshakeTimeout string `toml:"tls_handshake_timeout"`
	// PreAuthTimeout bounds the gap between handshake and successful
	// LOGIN; an unauthenticated connection idle past it is disconnected.
	PreAuthTimeout string `toml:"preauth_timeout"`
	// AuthVerifyBudgetMiB caps the total Argon2id scratch memory in flight
	// across the daemon (RA6X-023). Every LOGIN pays a KDF — including the
	// deliberate equalizing hash for an unknown or disabled account — so
	// without a global cap a distributed attacker holding no credentials can
	// commission unbounded memory-intensive work. 0 selects a default scaled
	// to GOMAXPROCS. It is a floor as well as a cap: a value below one
	// maximum-cost verification is raised, because a login that can never be
	// admitted is worse than a slow one.
	AuthVerifyBudgetMiB int `toml:"auth_verify_budget_mib"`
	// AuthMinDuration is the floor every LOGIN is padded to, so an account's
	// stored Argon2 cost cannot be read off the wire (RA6X-060). It is a lower
	// bound on a self-calibrating value: at startup the daemon times one
	// default-cost verification on this host and uses whichever is larger, so
	// a machine faster than whoever picked this number still gets a floor that
	// actually covers a default-cost account. Empty selects 150ms.
	AuthMinDuration string `toml:"auth_min_duration"`
}

// ArchiveConfig drives archive sorting (ARCHIVE_SORTING.md at the repository
// root): the live sorter that files what users archive into
// Archive/<category>, and the Trash purge that destroys what they delete. Both
// are off unless enabled, and both act on every mailbox that has the folder
// they need (\Archive plus active categories; \Trash).
type ArchiveConfig struct {
	// SortEnabled files classified messages out of each mailbox's \Archive
	// folder into their category folders.
	SortEnabled bool `toml:"sort_enabled"`
	// PurgeEnabled destroys messages that have been in each mailbox's \Trash
	// folder longer than TrashRetention.
	PurgeEnabled bool `toml:"purge_enabled"`
	// DeleteArchives makes Delete mean Archive, as Gmail does: a message a
	// client marks \Deleted and expunges anywhere but \Trash, \Drafts and
	// \Junk is archived instead of destroyed when it is the last copy, and a
	// \Deleted message left in INBOX is archived after SettleDelay.
	DeleteArchives bool `toml:"delete_archives"`
	// Interval between passes.
	Interval string `toml:"interval"`
	// SettleDelay leaves a just-archived message alone for this long, so a
	// mail client's Undo (a MOVE back by UID) still finds it.
	SettleDelay string `toml:"settle_delay"`
	// MinConfidence is the lowest classifier confidence that files a message.
	MinConfidence float64 `toml:"min_confidence"`
	// TrashRetention is how long a deleted message can still be recovered
	// from Trash before it is destroyed.
	TrashRetention string `toml:"trash_retention"`
	// PurgeAllCopies also destroys every other copy of a purged message's
	// content in the same mailbox.
	PurgeAllCopies bool `toml:"purge_all_copies"`
	// BatchSize is how many messages one transaction files or destroys.
	BatchSize int `toml:"batch_size"`
}

type LoggingConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

func DefaultConfig() *Config {
	return &Config{
		Production: false,
		Server: ServerConfig{
			ListenAddr:      ":993",
			MinTLSVersion:   "1.3",
			DrainTimeoutSec: 30,
		},
		Admin: AdminConfig{
			ListenAddr:         "127.0.0.1:8783",
			ExposeMetrics:      true,
			ReadTimeoutSec:     5,
			WriteTimeoutSec:    10,
			ShutdownTimeoutSec: 30,
		},
		Postgres: PostgresConfig{
			StatementTimeout: "10s",
			MaxOpenConns:     40,
			MaxIdleConns:     10,
			ConnMaxLifetime:  "5m",
			IdleConns:        20,
		},
		Storage: StorageConfig{
			Root: "/var/spool/epistula-database",
		},
		Limits: LimitsConfig{
			PerIPConnections:      10,
			PerMailboxConnections: 5,
			LoginAttempts:         5,
			LoginWindow:           "60s",
			LoginCooldown:         "60s",
			CommandRatePerSec:     100,
			IdleTimeout:           "29m",
			MaxAppendBytes:        52_428_800,
			MaxSearchResults:      100_000,
			TLSHandshakeTimeout:   "10s",
			PreAuthTimeout:        "60s",
			AuthMinDuration:       "150ms",
		},
		Archive: ArchiveConfig{
			Interval:       "1m",
			SettleDelay:    "5m",
			MinConfidence:  0.6,
			TrashRetention: "24h",
			PurgeAllCopies: true,
			BatchSize:      500,
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
	// no TOML existed, so an operator who set IMAP_DATABASE_* alongside a config file saw
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
	if v := os.Getenv("IMAP_DATABASE_DSN"); v != "" {
		cfg.Postgres.DSN = v
	}
	if v := os.Getenv("IMAP_DATABASE_STORAGE_ROOT"); v != "" {
		cfg.Storage.Root = v
	}
	if v := os.Getenv("IMAP_DATABASE_TLS_CERT"); v != "" {
		cfg.Server.TLSCert = v
	}
	if v := os.Getenv("IMAP_DATABASE_TLS_KEY"); v != "" {
		cfg.Server.TLSKey = v
	}
	if v := os.Getenv("IMAP_DATABASE_LISTEN_ADDR"); v != "" {
		cfg.Server.ListenAddr = v
	}
	if v := os.Getenv("IMAP_DATABASE_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("IMAP_DATABASE_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv("IMAP_DATABASE_PRODUCTION"); v == "true" || v == "1" {
		cfg.Production = true
	}
}

func (c *Config) Validate() error {
	if c.Postgres.DSN == "" {
		return errors.New("postgres.dsn is required")
	}
	// Durations are validated against what their CONSTRUCTOR does with them,
	// not merely for parseability (RA6X-050). A value that parses and then
	// selects a fallback, or disables a limit that has no "off" setting, is a
	// configuration that lies about what the daemon will do — and it only
	// shows up after the listeners are open.
	//
	// Zero has a documented meaning for the three keys where a fallback is the
	// intended behaviour (statement_timeout, conn_max_lifetime, idle_timeout);
	// it has none for a throttle window, so those must be positive.
	if err := nonNegativeDuration("postgres.statement_timeout", c.Postgres.StatementTimeout,
		"0 selects the built-in 10s per-command bound"); err != nil {
		return err
	}
	if err := nonNegativeDuration("postgres.conn_max_lifetime", c.Postgres.ConnMaxLifetime,
		"0 leaves pgx's own default in place"); err != nil {
		return err
	}
	if err := positiveDuration("limits.login_window", c.Limits.LoginWindow,
		"set limits.login_attempts = 0 to disable login throttling instead"); err != nil {
		return err
	}
	if err := positiveDuration("limits.login_cooldown", c.Limits.LoginCooldown,
		"set limits.login_attempts = 0 to disable login throttling instead"); err != nil {
		return err
	}
	if err := nonNegativeDuration("limits.idle_timeout", c.Limits.IdleTimeout,
		"0 selects the built-in 29m IDLE re-poll"); err != nil {
		return err
	}
	if d, err := time.ParseDuration(c.Limits.TLSHandshakeTimeout); err != nil || d <= 0 {
		return fmt.Errorf("limits.tls_handshake_timeout must be a positive duration: %q", c.Limits.TLSHandshakeTimeout)
	}
	if c.Limits.AuthVerifyBudgetMiB < 0 {
		return fmt.Errorf("limits.auth_verify_budget_mib must not be negative: %d", c.Limits.AuthVerifyBudgetMiB)
	}
	if c.Limits.AuthMinDuration != "" {
		if d, err := time.ParseDuration(c.Limits.AuthMinDuration); err != nil || d < 0 {
			return fmt.Errorf("limits.auth_min_duration must be a non-negative duration: %q", c.Limits.AuthMinDuration)
		}
	}
	if d, err := time.ParseDuration(c.Limits.PreAuthTimeout); err != nil || d <= 0 {
		return fmt.Errorf("limits.preauth_timeout must be a positive duration: %q", c.Limits.PreAuthTimeout)
	}
	if c.Storage.Root == "" {
		return errors.New("storage.root is required")
	}
	if c.Server.ListenAddr == "" {
		return errors.New("server.listen_addr is required")
	}
	if _, err := c.MinTLSVersion(); err != nil {
		return err
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
	if c.Limits.PerIPConnections <= 0 || c.Limits.PerMailboxConnections <= 0 {
		return errors.New("limits.per_*_connections must be positive")
	}
	if c.Limits.MaxSearchResults < 0 {
		return errors.New("limits.max_search_results must be non-negative (0 disables)")
	}
	if c.Limits.MaxAppendBytes <= 0 {
		return errors.New("limits.max_append_bytes must be positive")
	}

	// RO5X-032: keys that were entirely unvalidated.
	//
	// admin.listen_addr is the load-bearing one. With it empty and
	// production = false, http.Server{Addr: ""} binds :80 on EVERY interface —
	// publishing an unauthenticated /metrics + /debug/vars endpoint on the
	// public HTTP port. production = true happened to catch it via
	// isLoopbackAddr("") → false, but development and staging did not.
	if c.Admin.ListenAddr == "" {
		return errors.New("admin.listen_addr is required " +
			`(an empty address binds :80 on every interface, exposing /metrics and /debug/vars)`)
	}
	if c.Server.DrainTimeoutSec < 0 {
		return errors.New("server.drain_timeout_seconds must be non-negative")
	}
	if c.Limits.LoginAttempts < 0 {
		return errors.New("limits.login_attempts must be non-negative (0 disables)")
	}
	if c.Limits.CommandRatePerSec < 0 {
		return errors.New("limits.command_rate_per_sec must be non-negative (0 disables)")
	}
	if c.Postgres.MaxOpenConns < 1 {
		return errors.New("postgres.max_open_conns must be >= 1")
	}
	// max_idle_conns is pgxpool's MinConns — connections kept warm — so a
	// value above max_open_conns is a pool pgx refuses to build, and it did so
	// only at serve time, after check-config had passed and the listeners were
	// about to open (RA6X-050). The writer validates the same relationship;
	// this reader did not.
	if c.Postgres.MaxIdleConns < 0 {
		return fmt.Errorf("postgres.max_idle_conns must not be negative: %d (0 keeps no connection warm)",
			c.Postgres.MaxIdleConns)
	}
	if c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		return fmt.Errorf("postgres.max_idle_conns (%d) must not exceed postgres.max_open_conns (%d)",
			c.Postgres.MaxIdleConns, c.Postgres.MaxOpenConns)
	}
	// idle_conn_pool_size caps the dedicated LISTEN pool; 0 shares the main
	// query pool, which is the documented way to turn the dedicated pool off.
	// A negative value became a negative pgxpool MaxConns.
	if c.Postgres.IdleConns < 0 {
		return fmt.Errorf("postgres.idle_conn_pool_size must not be negative: %d "+
			"(0 shares the main query pool)", c.Postgres.IdleConns)
	}

	if err := c.Archive.validate(); err != nil {
		return err
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
	if c.Server.AcceptInsecure {
		return errors.New("server.accept_insecure_for_dev must be false in production")
	}
	if c.Server.TLSCert == "" || c.Server.TLSKey == "" {
		return errors.New("server.tls_cert and server.tls_key are required")
	}
	if _, err := os.Stat(c.Server.TLSCert); err != nil {
		return fmt.Errorf("server.tls_cert not readable: %w", err)
	}
	if _, err := os.Stat(c.Server.TLSKey); err != nil {
		return fmt.Errorf("server.tls_key not readable: %w", err)
	}
	if !isLoopbackAddr(c.Admin.ListenAddr) {
		return fmt.Errorf("admin.listen_addr %q must bind loopback (127.0.0.1/[::1]/localhost) — the metrics listener is unauthenticated and must never be exposed beyond the host", c.Admin.ListenAddr)
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

// MinTLSVersion returns the parsed crypto/tls version constant.
// The daemon refuses 1.0/1.1 regardless of configuration.
func (c *Config) MinTLSVersion() (uint16, error) {
	switch c.Server.MinTLSVersion {
	case "", "1.3":
		return tls.VersionTLS13, nil
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.0", "1.1":
		return 0, fmt.Errorf("server.min_tls_version=%q is refused (TLS 1.0/1.1 are deprecated)", c.Server.MinTLSVersion)
	default:
		return 0, fmt.Errorf("server.min_tls_version=%q is invalid (use 1.2 or 1.3)", c.Server.MinTLSVersion)
	}
}

// positiveDuration validates a duration key that has no meaningful zero. hint
// names the setting that actually disables the feature, so the error tells an
// operator what to do instead of only what is wrong (RA6X-050).
func positiveDuration(key, value, hint string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s must be a duration: %q (%s)", key, value, hint)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be positive: %q (%s)", key, value, hint)
	}
	return nil
}

// nonNegativeDuration validates a duration key whose zero selects a documented
// default. A negative value selects the same default silently, which is the
// behaviour this rejects.
func nonNegativeDuration(key, value, zeroMeaning string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s must be a duration: %q (%s)", key, value, zeroMeaning)
	}
	if d < 0 {
		return fmt.Errorf("%s must not be negative: %q (%s)", key, value, zeroMeaning)
	}
	return nil
}

// validate checks the [archive] keys whether or not either job is enabled, so
// a mistake is caught by check-config before anyone turns the job on.
func (a ArchiveConfig) validate() error {
	if err := positiveDuration("archive.interval", a.Interval,
		"set archive.sort_enabled and archive.purge_enabled = false to stop both jobs"); err != nil {
		return err
	}
	if err := nonNegativeDuration("archive.settle_delay", a.SettleDelay,
		"0 files an archived message on the next pass, before a client's Undo can reach it"); err != nil {
		return err
	}
	if err := nonNegativeDuration("archive.trash_retention", a.TrashRetention,
		"0 destroys whatever is in Trash on the next pass, with no chance to recover a mistaken delete"); err != nil {
		return err
	}
	if math.IsNaN(a.MinConfidence) || a.MinConfidence < 0 || a.MinConfidence > 1 {
		return fmt.Errorf("archive.min_confidence must be between 0 and 1: %v", a.MinConfidence)
	}
	if a.BatchSize < 1 || a.BatchSize > 5000 {
		return fmt.Errorf("archive.batch_size must be between 1 and 5000: %d", a.BatchSize)
	}
	return nil
}

// Durations returns the parsed [archive] durations. Validate has already
// checked them, so a parse failure here is a programming error.
func (a ArchiveConfig) Durations() (interval, settle, retention time.Duration) {
	interval, _ = time.ParseDuration(a.Interval)
	settle, _ = time.ParseDuration(a.SettleDelay)
	retention, _ = time.ParseDuration(a.TrashRetention)
	return interval, settle, retention
}
