package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigValidatesWhenDSNSet(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	if err := cfg.Validate(); err != nil {
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
	cases := []string{"sslmode=disable", "sslmode=allow", ""}
	for _, mode := range cases {
		cfg := DefaultConfig()
		cfg.Production = true
		if mode == "" {
			cfg.Postgres.DSN = "postgres://localhost/mail"
		} else {
			cfg.Postgres.DSN = "postgres://localhost/mail?" + mode
		}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("production should reject DSN with %q", mode)
		} else if !strings.Contains(err.Error(), "sslmode") {
			t.Errorf("expected sslmode error for %q, got %v", mode, err)
		}
	}
}

func TestProductionRequiresStrongerArgon2Memory(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
	cfg.Argon2.Memory = 12 * 1024
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "argon2.memory_kib") {
		t.Fatalf("expected argon2.memory_kib error, got %v", err)
	}
}

func TestProductionAcceptsVerifyFull(t *testing.T) {
	// sslrootcert names a file the connection layer opens while resolving the
	// DSN, so production validation now needs a readable one — the same
	// requirement the daemon has at startup (RA6X-029). The old fixed
	// /etc/ssl/ca.pem was never read by the substring check.
	ca := filepath.Join(t.TempDir(), "ca.pem")
	writeTestCA(t, ca)

	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full&sslrootcert=" + ca
	if err := cfg.Validate(); err != nil {
		t.Fatalf("verify-full DSN should validate: %v", err)
	}
}

// TestProductionRejectsDisguisedSSLMode is the RA6X-029 regression at the
// Config boundary: a percent-escaped sslmode with a decoy application_name
// satisfied both halves of the old substring guard while pgx connected in
// plaintext.
func TestProductionRejectsDisguisedSSLMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Postgres.DSN = "postgres://user@db.mail.invalid/mail?sslmode=%64isable&application_name=sslmode=verify-full"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("production accepted a DSN that resolves to sslmode=disable")
	}
	if !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("expected an unencrypted-connection error, got %v", err)
	}
}

// TestDevelopmentKeepsLocalPlaintextDSN pins that the stricter production
// guard did not leak into development mode, where a local plaintext DSN is
// the documented default.
func TestDevelopmentKeepsLocalPlaintextDSN(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Production = false
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development must accept a local plaintext DSN: %v", err)
	}
}

// writeTestCA generates a throwaway self-signed CA and writes it to path, so
// a verify-full DSN naming it actually resolves. It is not a credential: the
// private key is discarded and the certificate is never presented to anything.
func writeTestCA(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ra6x029-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epistula-database.toml")
	body := `
production = false
[postgres]
dsn = "postgres://localhost/mail?sslmode=disable"
[storage]
root = "/tmp/epistula-database-test"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Storage.Root != "/tmp/epistula-database-test" {
		t.Errorf("storage.root not loaded: %q", cfg.Storage.Root)
	}
}

func TestInvalidLogLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Logging.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid log level")
	}
}

func TestReferenceSchemaExcludesLegacyAdminTables(t *testing.T) {
	body, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	schema := string(body)
	for _, name := range []string{"admin_users", "admin_sessions"} {
		if strings.Contains(schema, name) {
			t.Fatalf("schema.sql still defines legacy table %s", name)
		}
	}
}

func TestProductionRequiresLoopbackHTTPListener(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8782", ":8782", "192.168.1.5:8782"} {
		cfg := DefaultConfig()
		cfg.Production = true
		cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
		cfg.HTTP.ListenAddr = addr
		if err := cfg.Validate(); err == nil {
			t.Errorf("production should reject non-loopback http.listen_addr %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8782", "[::1]:8782", "localhost:8782"} {
		cfg := DefaultConfig()
		cfg.Production = true
		cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=verify-full"
		cfg.HTTP.ListenAddr = addr
		if err := cfg.Validate(); err != nil {
			t.Errorf("production should accept loopback http.listen_addr %q: %v", addr, err)
		}
	}
}

func TestArgon2ParallelismFloor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail"
	cfg.Argon2.Parallel = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("argon2.parallelism = 0 must be rejected (argon2.IDKey panics on p=0)")
	}
}
