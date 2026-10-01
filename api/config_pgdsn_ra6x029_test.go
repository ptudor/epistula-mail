package main

import (
	"strings"
	"testing"
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
	cfg := DefaultConfig()
	cfg.Production = true
	cfg.Server.ListenAddr = "127.0.0.1:8784"
	cfg.Admin.ListenAddr = "127.0.0.1:8785"
	return cfg
}
