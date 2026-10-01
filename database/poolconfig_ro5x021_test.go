package main

import (
	"strings"
	"testing"
)

// TestPoolConfigValidation is the RO5X-021 validation half: the documented
// tuning knobs are now honoured by serve/gc, so a nonsensical value must be
// rejected at check-config rather than silently producing a broken pool.
func TestPoolConfigValidation(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.Postgres.DSN = "postgres://localhost/x?sslmode=disable"
		c.Storage.Root = t.TempDir()
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}

	for _, tc := range []struct {
		name      string
		mutate    func(*Config)
		wantMatch string
	}{
		{"zero max_open_conns", func(c *Config) { c.Postgres.MaxOpenConns = 0 }, "max_open_conns"},
		{"negative max_open_conns", func(c *Config) { c.Postgres.MaxOpenConns = -1 }, "max_open_conns"},
		{"negative max_idle_conns", func(c *Config) { c.Postgres.MaxIdleConns = -1 }, "max_idle_conns"},
		{"idle above open", func(c *Config) {
			c.Postgres.MaxOpenConns = 4
			c.Postgres.MaxIdleConns = 5
		}, "max_idle_conns"},
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

	// The boundary is legal: idle == open.
	c := base()
	c.Postgres.MaxOpenConns = 4
	c.Postgres.MaxIdleConns = 4
	if err := c.Validate(); err != nil {
		t.Errorf("max_idle_conns == max_open_conns should be legal: %v", err)
	}
	// And zero idle connections is legal (no warm pool).
	c = base()
	c.Postgres.MaxIdleConns = 0
	if err := c.Validate(); err != nil {
		t.Errorf("max_idle_conns = 0 should be legal: %v", err)
	}
}
