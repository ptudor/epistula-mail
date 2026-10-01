package main

import "testing"

// validConfig returns a DefaultConfig with a DSN so Validate passes; tests then
// mutate a single field to assert the R-030 rejections.
func validConfig() *Config {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	return cfg
}

// TestValidateRejectsBadLimitsAndArgon is the R-030 regression: typos that
// would turn into a full outage or a bounce-storm are rejected at startup.
func TestValidateRejectsBadLimitsAndArgon(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero delivery.timeout", func(c *Config) { c.Delivery.Timeout = "0s" }},
		{"negative delivery.timeout", func(c *Config) { c.Delivery.Timeout = "-5s" }},
		{"zero max_header_bytes", func(c *Config) { c.Limits.MaxHeaderBytes = 0 }},
		{"zero max_header_section_bytes", func(c *Config) { c.Limits.MaxHeaderSectionBytes = 0 }},
		{"section smaller than header", func(c *Config) {
			c.Limits.MaxHeaderBytes = 16384
			c.Limits.MaxHeaderSectionBytes = 8192
		}},
		{"weak salt_len", func(c *Config) { c.Argon2.SaltLen = 4 }},
		{"weak key_len", func(c *Config) { c.Argon2.KeyLen = 16 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted an invalid config (%s)", tc.name)
			}
		})
	}
}

// TestValidateAcceptsDefaults sanity-checks that the defaults still pass after
// the new checks (guards against an over-tight rule).
func TestValidateAcceptsDefaults(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

// TestGCRejectsNegativeGrace is the gc.go half of R-030.
func TestGCRejectsNegativeGrace(t *testing.T) {
	if code := runGC([]string{"-phase", "mark", "-grace-seconds", "-5", "-config", "/nonexistent"}); code != EX_USAGE {
		t.Fatalf("gc -grace-seconds=-5 exit = %d, want EX_USAGE (%d)", code, EX_USAGE)
	}
}
