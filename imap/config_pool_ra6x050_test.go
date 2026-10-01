package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPoolAndThrottleBoundsAreValidated is the RA6X-050 regression.
//
// Validate checked only that several durations PARSED and that
// max_open_conns was at least one. Everything else about the pool and the
// throttles was accepted and then reinterpreted by its constructor: a
// max_idle_conns above max_open_conns is a pool pgx refuses to build (at serve
// time, after check-config had passed), a negative idle_conn_pool_size becomes
// a negative pgxpool MaxConns, and a zero or negative login window is a
// throttle that does not throttle. The writer validates the same pool
// relationships; this reader did not.
func TestPoolAndThrottleBoundsAreValidated(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.Postgres.DSN = "postgres://localhost/x?sslmode=disable"
		c.Storage.Root = t.TempDir()
		c.Server.TLSCert = ""
		c.Server.TLSKey = ""
		c.Server.AcceptInsecure = true
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the baseline config is not valid, so this test proves nothing: %v", err)
	}

	rejected := []struct {
		name    string
		mutate  func(*Config)
		wantKey string
	}{
		{"idle conns above open conns", func(c *Config) {
			c.Postgres.MaxOpenConns = 4
			c.Postgres.MaxIdleConns = 8
		}, "max_idle_conns"},
		{"negative idle conns", func(c *Config) { c.Postgres.MaxIdleConns = -1 }, "max_idle_conns"},
		{"negative dedicated idle pool", func(c *Config) { c.Postgres.IdleConns = -1 }, "idle_conn_pool_size"},
		{"zero login window", func(c *Config) { c.Limits.LoginWindow = "0s" }, "login_window"},
		{"negative login window", func(c *Config) { c.Limits.LoginWindow = "-30s" }, "login_window"},
		{"unparseable login window", func(c *Config) { c.Limits.LoginWindow = "sixty" }, "login_window"},
		{"zero login cooldown", func(c *Config) { c.Limits.LoginCooldown = "0" }, "login_cooldown"},
		{"negative login cooldown", func(c *Config) { c.Limits.LoginCooldown = "-1m" }, "login_cooldown"},
		{"negative conn lifetime", func(c *Config) { c.Postgres.ConnMaxLifetime = "-5m" }, "conn_max_lifetime"},
		{"negative statement timeout", func(c *Config) { c.Postgres.StatementTimeout = "-10s" }, "statement_timeout"},
		{"negative idle timeout", func(c *Config) { c.Limits.IdleTimeout = "-29m" }, "idle_timeout"},
		{"unparseable idle timeout", func(c *Config) { c.Limits.IdleTimeout = "half an hour" }, "idle_timeout"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name the bad key %q", err, tc.wantKey)
			}
		})
	}

	// Documented zero settings and the shipped defaults still validate.
	accepted := map[string]func(*Config){
		"no connection kept warm":       func(c *Config) { c.Postgres.MaxIdleConns = 0 },
		"idle pool shares the main one": func(c *Config) { c.Postgres.IdleConns = 0 },
		"pgx default conn lifetime":     func(c *Config) { c.Postgres.ConnMaxLifetime = "0s" },
		"built-in statement timeout":    func(c *Config) { c.Postgres.StatementTimeout = "0s" },
		"built-in idle re-poll":         func(c *Config) { c.Limits.IdleTimeout = "0s" },
		"login throttle disabled":       func(c *Config) { c.Limits.LoginAttempts = 0 },
		"command rate disabled":         func(c *Config) { c.Limits.CommandRatePerSec = 0 },
		"search cap disabled":           func(c *Config) { c.Limits.MaxSearchResults = 0 },
		"idle conns equal open conns":   func(c *Config) { c.Postgres.MaxIdleConns = c.Postgres.MaxOpenConns },
	}
	for name, mutate := range accepted {
		t.Run("accepted: "+name, func(t *testing.T) {
			c := base()
			mutate(c)
			if err := c.Validate(); err != nil {
				t.Errorf("Validate rejected a documented setting (%s): %v", name, err)
			}
		})
	}
}

// TestCheckConfigAndServeAgreeWithoutBinding pins that a bad pool relationship
// is caught by check-config, at the same gate serve uses, before any listener
// is opened. Both subcommands reach Validate through LoadConfig and nothing
// else, so a config that check-config accepts is one serve will accept too.
func TestCheckConfigAndServeAgreeWithoutBinding(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "epistula-imap.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	root := t.TempDir()

	bad := write(`
production = false

[server]
listen_addr = "127.0.0.1:0"
accept_insecure_for_dev = true

[postgres]
dsn = "postgres://localhost/x?sslmode=disable"
max_open_conns = 4
max_idle_conns = 8

[storage]
root = "` + root + `"
`)
	if _, err := LoadConfig(bad); err == nil {
		t.Error("LoadConfig accepted max_idle_conns above max_open_conns")
	} else if !strings.Contains(err.Error(), "max_idle_conns") {
		t.Errorf("error %q does not name the bad key", err)
	}
	// check-config reports the same refusal, and returns the config exit code
	// rather than starting anything.
	if code := runCheckConfig([]string{"-config", bad}); code != EX_CONFIG {
		t.Errorf("check-config returned %d for an invalid config, want EX_CONFIG (%d)", code, EX_CONFIG)
	}

	good := write(`
production = false

[server]
listen_addr = "127.0.0.1:0"
accept_insecure_for_dev = true

[postgres]
dsn = "postgres://localhost/x?sslmode=disable"
max_open_conns = 8
max_idle_conns = 4
idle_conn_pool_size = 0

[storage]
root = "` + root + `"
`)
	if _, err := LoadConfig(good); err != nil {
		t.Fatalf("LoadConfig rejected a valid config: %v", err)
	}
	if code := runCheckConfig([]string{"-config", good}); code != EX_OK {
		t.Errorf("check-config returned %d for a valid config, want EX_OK (%d)", code, EX_OK)
	}
}
