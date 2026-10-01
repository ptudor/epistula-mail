package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempConfig writes a TOML config to a temp file and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "epistula-mcp.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearEnv removes every MAIL_MCP_* override for the duration of a test so the
// process environment can't leak into a case that means to test the file.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MAIL_MCP_BASE_URL", "MAIL_MCP_TOKEN", "MAIL_MCP_HTTP_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestConfigDefaults(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784"
token = "tok-abc"
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want %v", cfg.RequestTimeout, defaultRequestTimeout)
	}
	if cfg.MaxLimit != defaultMaxLimit {
		t.Errorf("MaxLimit = %d, want %d", cfg.MaxLimit, defaultMaxLimit)
	}
	if cfg.MaxTextBytes != defaultMaxTextBytes {
		t.Errorf("MaxTextBytes = %d, want %d", cfg.MaxTextBytes, defaultMaxTextBytes)
	}
	if cfg.BaseURL != "http://127.0.0.1:8784" || cfg.Token != "tok-abc" {
		t.Errorf("got BaseURL=%q Token=%q", cfg.BaseURL, cfg.Token)
	}
}

func TestConfigCustomCaps(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "https://epistula-api.example.invalid"
token = "tok-abc"
request_timeout = "10s"

[mcp]
max_limit = 50
max_text_bytes = 1024
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout != 10*time.Second {
		t.Errorf("RequestTimeout = %v, want 10s", cfg.RequestTimeout)
	}
	if cfg.MaxLimit != 50 {
		t.Errorf("MaxLimit = %d, want 50", cfg.MaxLimit)
	}
	if cfg.MaxTextBytes != 1024 {
		t.Errorf("MaxTextBytes = %d, want 1024", cfg.MaxTextBytes)
	}
}

func TestConfigEnvOverridesFile(t *testing.T) {
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784"
token = "file-token"
`)
	t.Setenv("MAIL_MCP_BASE_URL", "https://env.example.invalid")
	t.Setenv("MAIL_MCP_TOKEN", "env-token")
	t.Setenv("MAIL_MCP_HTTP_TOKEN", "env-http")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://env.example.invalid" {
		t.Errorf("BaseURL = %q, want the env value", cfg.BaseURL)
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token = %q, want the env value", cfg.Token)
	}
	if cfg.HTTPToken != "env-http" {
		t.Errorf("HTTPToken = %q, want the env value", cfg.HTTPToken)
	}
}

func TestConfigTrailingSlashTrimmed(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784/"
token = "t"
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://127.0.0.1:8784" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", cfg.BaseURL)
	}
}

func TestConfigHTTPSRequiredForNonLoopback(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://epistula-api.example.invalid"
token = "t"
`)
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("expected error for cleartext non-loopback base_url")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error should explain the https requirement, got %q", err)
	}
}

func TestConfigHTTPLoopbackAllowed(t *testing.T) {
	clearEnv(t)
	for _, host := range []string{"http://127.0.0.1:8784", "http://localhost:8784", "http://[::1]:8784"} {
		p := writeTempConfig(t, "[mailapi]\nbase_url = \""+host+"\"\ntoken = \"t\"\n")
		if _, err := loadConfig(p); err != nil {
			t.Errorf("loopback %s should be allowed over http, got %v", host, err)
		}
	}
}

func TestConfigHTTPSNonLoopbackAllowed(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "https://epistula-api.example.invalid"
token = "t"
`)
	if _, err := loadConfig(p); err != nil {
		t.Errorf("https non-loopback should be allowed, got %v", err)
	}
}

func TestConfigMissingTokenErrors(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784"
`)
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should mention the token, got %q", err)
	}
}

// TestConfigErrorsAreSecretFree checks that neither the bearer token nor any
// userinfo password embedded in base_url leaks into a returned error string.
func TestConfigErrorsAreSecretFree(t *testing.T) {
	clearEnv(t)
	// A cleartext non-loopback base_url with embedded creds triggers the
	// https-required error; the password must not appear in it.
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://user:supersecretpw@epistula-api.example.invalid"
token = "topsecrettoken"
`)
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "supersecretpw") {
		t.Errorf("error leaked the base_url password: %q", err)
	}
	if strings.Contains(err.Error(), "topsecrettoken") {
		t.Errorf("error leaked the bearer token: %q", err)
	}
}

func TestConfigBadTimeout(t *testing.T) {
	clearEnv(t)
	p := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784"
token = "t"
request_timeout = "-5s"
`)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("expected error for negative request_timeout")
	}
}
