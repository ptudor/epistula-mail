package main

import (
	"strings"
	"testing"
)

// TestValidateRejectsUnvalidatedKeys is the RO5X-032 regression.
//
// Validate did not check admin.listen_addr, server.drain_timeout_seconds,
// limits.login_attempts, limits.command_rate_per_sec, or
// postgres.max_open_conns. The first is load-bearing: with
// admin.listen_addr = "" and production = false, http.Server{Addr: ""} binds
// :80 on every interface, publishing unauthenticated /metrics and
// /debug/vars on the public HTTP port.
func TestValidateRejectsUnvalidatedKeys(t *testing.T) {
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
		t.Fatalf("baseline config should validate: %v", err)
	}

	for _, tc := range []struct {
		name      string
		mutate    func(*Config)
		wantMatch string
	}{
		{"empty admin.listen_addr", func(c *Config) { c.Admin.ListenAddr = "" }, "admin.listen_addr"},
		{"negative drain timeout", func(c *Config) { c.Server.DrainTimeoutSec = -1 }, "drain_timeout_seconds"},
		{"negative login_attempts", func(c *Config) { c.Limits.LoginAttempts = -1 }, "login_attempts"},
		{"negative command_rate_per_sec", func(c *Config) { c.Limits.CommandRatePerSec = -1 }, "command_rate_per_sec"},
		{"negative max_search_results", func(c *Config) { c.Limits.MaxSearchResults = -1 }, "max_search_results"},
		{"zero max_open_conns", func(c *Config) { c.Postgres.MaxOpenConns = 0 }, "max_open_conns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Errorf("err = %v, want it to name %s", err, tc.wantMatch)
			}
		})
	}
}

// TestValidateAcceptsDisablingZeros keeps the documented "0 disables"
// semantics legal — the values warn at startup rather than failing validation.
func TestValidateAcceptsDisablingZeros(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.Postgres.DSN = "postgres://localhost/x?sslmode=disable"
		c.Storage.Root = t.TempDir()
		c.Server.AcceptInsecure = true
		return c
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"login_attempts = 0", func(c *Config) { c.Limits.LoginAttempts = 0 }},
		{"command_rate_per_sec = 0", func(c *Config) { c.Limits.CommandRatePerSec = 0 }},
		{"max_search_results = 0", func(c *Config) { c.Limits.MaxSearchResults = 0 }},
		{"drain_timeout_seconds = 0", func(c *Config) { c.Server.DrainTimeoutSec = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			if err := c.Validate(); err != nil {
				t.Errorf("Validate rejected the documented disabling value: %v", err)
			}
		})
	}
}
