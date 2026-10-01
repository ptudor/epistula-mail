package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvOverridesConfigFile is the RO5X-031 regression.
//
// Env used to be a FALLBACK consulted only when no TOML was found, so an
// operator who set IMAP_DATABASE_DSN alongside a config file saw it silently ignored —
// while llm-worker and epistula-mcp, which always override, honoured theirs. Three
// daemons treating env as fallback and two as override is a live hazard in a
// deployment that mixes both, especially for secrets.
//
// Precedence is now uniform everywhere: env > file > defaults.
func TestEnvOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epistula-imap.toml")
	if err := os.WriteFile(path, []byte("[postgres]\ndsn = \"postgres://file/db\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Without the env var, the file wins over the default.
	t.Setenv("IMAP_DATABASE_DSN", "")
	os.Unsetenv("IMAP_DATABASE_DSN")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Postgres.DSN != "postgres://file/db" {
		t.Errorf("DSN = %q, want the file value", cfg.Postgres.DSN)
	}

	// With it set, env wins over the file.
	t.Setenv("IMAP_DATABASE_DSN", "postgres://env/db")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Postgres.DSN != "postgres://env/db" {
		t.Errorf("DSN = %q, want the env value — env must override the file (RO5X-031)", cfg.Postgres.DSN)
	}
}
