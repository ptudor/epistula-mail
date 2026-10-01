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

// TestProductionRejectsDisguisedSSLMode is the RA6X-029 regression: the
// production guard must read the SSL mode pgx resolves, not search the DSN's
// text. "%64isable" is "disable" percent-escaped, so the old negative
// substring never matched, and the decoy application_name satisfied the
// positive one — while the connection ran in plaintext.
func TestProductionRejectsDisguisedSSLMode(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "percent-escaped disable with decoy application_name",
			dsn:  "postgres://user@db.mail.invalid/mail?sslmode=%64isable&application_name=sslmode=verify-full",
			want: "unencrypted",
		},
		{
			name: "default prefer with decoy application_name",
			dsn:  "postgres://user@db.mail.invalid/mail?application_name=sslmode=verify-full",
			want: "unencrypted",
		},
		{
			name: "require without a root cert leaves the peer unauthenticated",
			dsn:  "postgres://user@db.mail.invalid/mail?sslmode=require",
			want: "does not verify",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := productionFixture(t)
			cfg.Postgres.DSN = tc.dsn
			err := cfg.Validate()
			if err == nil {
				t.Fatal("production accepted an unverified PostgreSQL connection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error mentioning %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), tc.dsn) {
				t.Fatalf("error echoes the DSN verbatim: %v", err)
			}
		})
	}
}

// TestProductionAcceptsVerifiedTLS pins the other direction: a correctly
// configured certificate-verified DSN must still validate, including one whose
// application_name or password contains text that trips a naive substring
// check.
func TestProductionAcceptsVerifiedTLS(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user@db.mail.invalid/mail?sslmode=verify-full",
		"postgres://user@db.mail.invalid/mail?sslmode=verify-ca",
		"host=db.mail.invalid user=app dbname=mail sslmode=verify-full",
		"postgres://user@db.mail.invalid/mail?sslmode=verify-full&application_name=sslmode=disable",
		"postgres://user@/mail?host=/var/run/postgresql",
	} {
		cfg := productionFixture(t)
		cfg.Postgres.DSN = dsn
		if err := cfg.Validate(); err != nil {
			t.Errorf("production rejected a verified DSN %q: %v", dsn, err)
		}
	}
}

// TestDevelopmentKeepsLocalPlaintextDSN pins that the stricter production
// guard did not leak into development mode, where a local plaintext DSN is
// the documented default.
func TestDevelopmentKeepsLocalPlaintextDSN(t *testing.T) {
	cfg := productionFixture(t)
	cfg.Production = false
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development must accept a local plaintext DSN: %v", err)
	}
}

// productionFixture returns a production Config that satisfies every
// production check other than the PostgreSQL DSN, so a test can vary the DSN
// alone.
func productionFixture(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeTestKeyPair(t, certPath, keyPath)

	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Server.AcceptInsecure = false
	cfg.Server.TLSCert = certPath
	cfg.Server.TLSKey = keyPath
	cfg.Admin.ListenAddr = "127.0.0.1:8783"
	return cfg
}

// writeTestKeyPair generates a throwaway self-signed certificate and key so
// the production TLS-path checks have real files to stat. Neither is a
// credential: both live only in the test's temporary directory.
func writeTestKeyPair(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ra6x029-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	der, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
