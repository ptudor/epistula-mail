package main

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigRejectsLegacyTLS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=disable"
	cfg.Server.MinTLSVersion = "1.1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error rejecting TLS 1.1")
	}
}

func TestDefaultConfigAcceptsTLS13(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=disable"
	v, err := cfg.MinTLSVersion()
	if err != nil {
		t.Fatalf("MinTLSVersion: %v", err)
	}
	if v != tls.VersionTLS13 {
		t.Errorf("default MinTLSVersion = %v, want TLS 1.3", v)
	}
}

func TestDefaultConnectionLimitsAccommodateClientPools(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Limits.PerMailboxConnections < 20 {
		t.Errorf("default per-mailbox connection limit = %d, want at least 20 for multi-device client pools",
			cfg.Limits.PerMailboxConnections)
	}
	if cfg.Limits.PerIPConnections <= cfg.Limits.PerMailboxConnections {
		t.Errorf("default per-IP connection limit = %d, want more than per-mailbox limit %d for shared NAT addresses",
			cfg.Limits.PerIPConnections, cfg.Limits.PerMailboxConnections)
	}
}

func TestDistributedConnectionLimitsMatchDefaults(t *testing.T) {
	want := DefaultConfig().Limits
	for _, path := range []string{
		"epistula-imap.toml.example",
		"../packaging/epistula-imap.toml",
	} {
		t.Run(path, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := loadTOML(path, cfg); err != nil {
				t.Fatalf("loadTOML: %v", err)
			}
			if cfg.Limits.PerMailboxConnections != want.PerMailboxConnections {
				t.Errorf("per_mailbox_connections = %d, want default %d",
					cfg.Limits.PerMailboxConnections, want.PerMailboxConnections)
			}
			if cfg.Limits.PerIPConnections != want.PerIPConnections {
				t.Errorf("per_ip_connections = %d, want default %d",
					cfg.Limits.PerIPConnections, want.PerIPConnections)
			}
		})
	}
}

func TestExplicitTLS12OptInMapsToVersionTLS12(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=disable"
	cfg.Server.MinTLSVersion = "1.2"
	v, err := cfg.MinTLSVersion()
	if err != nil {
		t.Fatalf("MinTLSVersion(1.2): %v", err)
	}
	if v != tls.VersionTLS12 {
		t.Errorf("MinTLSVersion = %v, want TLS 1.2 on explicit opt-in", v)
	}
}

func TestAEADCipherSuitesAreAEADOnly(t *testing.T) {
	// The 1.2 allowlist must contain exactly the six ECDHE+AEAD suites
	// from CLAUDE.md — no CBC, no static-RSA key exchange.
	want := map[uint16]bool{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256:       true,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:         true,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384:       true,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:         true,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256: true,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:   true,
	}
	if len(aeadCipherSuites) != len(want) {
		t.Fatalf("aeadCipherSuites has %d entries, want %d", len(aeadCipherSuites), len(want))
	}
	for _, id := range aeadCipherSuites {
		if !want[id] {
			t.Errorf("unexpected cipher suite %#04x in AEAD allowlist", id)
		}
	}
}

func TestProductionRefusesAcceptInsecure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=verify-full"
	cfg.Server.AcceptInsecure = true
	cfg.Server.TLSCert = "/nonexistent/cert.pem"
	cfg.Server.TLSKey = "/nonexistent/key.pem"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "accept_insecure") {
		t.Fatalf("expected accept_insecure error, got %v", err)
	}
}

func TestProductionRequiresTLSPaths(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=verify-full"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("expected tls_cert error, got %v", err)
	}
}

func TestProductionRefusesInsecureSSLMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=disable"
	cfg.Server.TLSCert = "/nonexistent/cert.pem"
	cfg.Server.TLSKey = "/nonexistent/key.pem"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("expected sslmode error, got %v", err)
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epistula-imap.toml")
	body := `
[server]
listen_addr = ":1993"
min_tls_version = "1.3"
[postgres]
dsn = "postgres://localhost/m?sslmode=disable"
[storage]
root = "/tmp/imap-test"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.ListenAddr != ":1993" {
		t.Errorf("server.listen_addr not loaded: %q", cfg.Server.ListenAddr)
	}
	if cfg.Storage.Root != "/tmp/imap-test" {
		t.Errorf("storage.root not loaded: %q", cfg.Storage.Root)
	}
}
