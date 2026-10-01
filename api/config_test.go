package main

import (
	"strings"
	"testing"
)

func validBase() *Config {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	return cfg
}

func TestDefaultConfigValidatesWhenDSNSet(t *testing.T) {
	if err := validBase().Validate(); err != nil {
		t.Fatalf("default config with DSN should validate: %v", err)
	}
}

func TestDefaultConfigRequiresDSN(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when postgres.dsn is empty")
	}
}

func TestProductionRefusesInsecureSSLMode(t *testing.T) {
	cfg := validBase()
	cfg.Production = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("production must reject sslmode=disable, got %v", err)
	}
}

func TestProductionRequiresLoopbackListeners(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8784", ":8784", "10.0.0.5:8784"} {
		cfg := validBase()
		cfg.Production = true
		cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
		cfg.Server.ListenAddr = addr
		if err := cfg.Validate(); err == nil {
			t.Errorf("production should reject non-loopback server.listen_addr %q", addr)
		}
		cfg = validBase()
		cfg.Production = true
		cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
		cfg.Admin.ListenAddr = addr
		if err := cfg.Validate(); err == nil {
			t.Errorf("production should reject non-loopback admin.listen_addr %q", addr)
		}
	}
	cfg := validBase()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
	if err := cfg.Validate(); err != nil {
		t.Errorf("production with loopback defaults should validate: %v", err)
	}
}

func TestPageSizeBoundsValidated(t *testing.T) {
	cfg := validBase()
	cfg.Limits.MaxPageSize = cfg.Limits.DefaultPageSize - 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("max_page_size below default_page_size must be rejected")
	}
	cfg = validBase()
	cfg.Limits.DefaultPageSize = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("zero default_page_size must be rejected")
	}
}

func TestAuthDurationsValidated(t *testing.T) {
	cfg := validBase()
	cfg.Limits.AuthFailWindow = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Fatal("bad auth_fail_window must be rejected")
	}
	cfg = validBase()
	cfg.Limits.AuthCacheTTL = "-5s"
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative auth_cache_ttl must be rejected")
	}
}
