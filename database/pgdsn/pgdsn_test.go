package pgdsn

import (
	"os"
	"strings"
	"testing"
)

// TestRequireVerifiedTLS is the RA6X-029 table: the production guard must
// track the SSL mode pgx will actually use, not the DSN's text.
func TestRequireVerifiedTLS(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
		// wantContains, when set, pins the operator-facing reason.
		wantContains string
	}{
		// --- the reported bypass -------------------------------------------
		{
			// RA6X-029's example. "%64isable" is "disable" percent-escaped, so
			// the old negative substring test never fired; the positive test
			// was satisfied by the application_name. pgx resolves this to
			// plaintext.
			name:         "percent-escaped disable hidden behind a decoy application_name",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=%64isable&application_name=sslmode=verify-full",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// Default mode is `prefer`, which carries a plaintext fallback.
			// The decoy application_name satisfied the old positive test.
			name:         "default prefer with a decoy application_name",
			dsn:          "postgres://user@db.mail.invalid/mail?application_name=sslmode=verify-full",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			name:         "decoy in the password",
			dsn:          "postgres://user:sslmode%3Dverify-full@db.mail.invalid/mail?sslmode=disable",
			wantErr:      true,
			wantContains: "unencrypted",
		},

		// --- modes that must be refused ------------------------------------
		{
			name:         "sslmode=disable",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=disable",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// allow returns [nil, tlsConfig] — plaintext is tried FIRST, so
			// checking only the head of the fallback list is not enough.
			name:         "sslmode=allow tries plaintext first",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=allow",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// prefer returns [tlsConfig, nil] — plaintext is the fallback, so
			// checking only the head of the list is not enough either.
			name:         "sslmode=prefer falls back to plaintext",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=prefer",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// Encrypted but unauthenticated: any MITM with a self-signed cert
			// reads the credentials and the mail.
			name:         "sslmode=require without a root cert does not verify the peer",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=require",
			wantErr:      true,
			wantContains: "does not verify",
		},

		// --- modes that must be accepted -----------------------------------
		{
			name:    "sslmode=verify-full",
			dsn:     "postgres://user@db.mail.invalid/mail?sslmode=verify-full",
			wantErr: false,
		},
		{
			name:    "sslmode=verify-ca",
			dsn:     "postgres://user@db.mail.invalid/mail?sslmode=verify-ca",
			wantErr: false,
		},

		// --- DSN spellings and precedence ----------------------------------
		{
			name:    "keyword DSN, verify-full",
			dsn:     "host=db.mail.invalid user=app dbname=mail sslmode=verify-full",
			wantErr: false,
		},
		{
			name:         "keyword DSN, disable",
			dsn:          "host=db.mail.invalid user=app dbname=mail sslmode=disable",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			name: "keyword DSN with a quoted decoy password",
			dsn:  `host=db.mail.invalid user=app password='sslmode=verify-full' dbname=mail sslmode=disable`,
			// The password quotes the positive substring; only real parsing
			// gets this right.
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// A repeated URL parameter has to be resolved the same way the
			// connection layer resolves it, whichever way that is. pgx's URL
			// parser takes the FIRST occurrence (pgconn.parseURLSettings reads
			// url.Query()[k][0]), so these two DSNs must be judged by their
			// first sslmode, not by whether "verify-full" appears anywhere.
			name:    "repeated sslmode, pgx takes the first and it is secure",
			dsn:     "postgres://user@db.mail.invalid/mail?sslmode=verify-full&sslmode=disable",
			wantErr: false,
		},
		{
			name:         "repeated sslmode, pgx takes the first and it is insecure",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=disable&sslmode=verify-full",
			wantErr:      true,
			wantContains: "unencrypted",
		},

		// --- Unix sockets: documented as acceptable ------------------------
		{
			name:    "unix socket directory host",
			dsn:     "postgres://user@/mail?host=/var/run/postgresql",
			wantErr: false,
		},
		{
			name:    "unix socket via keyword DSN",
			dsn:     "host=/tmp user=app dbname=mail",
			wantErr: false,
		},

		// --- input handling ------------------------------------------------
		{
			name:         "empty DSN",
			dsn:          "",
			wantErr:      true,
			wantContains: "empty",
		},
		{
			name:         "unparseable DSN",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=nonsense",
			wantErr:      true,
			wantContains: "not usable by the connection layer",
		},
		{
			// An sslrootcert the daemon cannot read means the connection
			// layer will refuse to start. check-config should say so rather
			// than let the DSN look secure on paper.
			name:         "verify-full with an unreadable root certificate",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=verify-full&sslrootcert=/nonexistent/ra6x029/ca.pem",
			wantErr:      true,
			wantContains: "not usable by the connection layer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireVerifiedTLS(tc.dsn)
			if tc.wantErr && err == nil {
				t.Fatalf("RequireVerifiedTLS(%q) = nil; want an error", tc.dsn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RequireVerifiedTLS(%q) = %v; want nil", tc.dsn, err)
			}
			if err != nil {
				if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
					t.Fatalf("error %q does not mention %q", err, tc.wantContains)
				}
				// A DSN carries a password; it must never reach the operator's
				// log through this error, in whole or in part.
				if tc.dsn != "" && strings.Contains(err.Error(), tc.dsn) {
					t.Fatalf("error %q echoes the DSN verbatim", err)
				}
				if strings.Contains(err.Error(), "sslmode%3Dverify-full") ||
					strings.Contains(err.Error(), "user:") {
					t.Fatalf("error %q leaks credentials", err)
				}
			}
		})
	}
}

// TestRequireVerifiedTLSHonoursEnvironment pins that the guard sees the same
// PG* environment defaults the connection layer does. An operator who sets
// PGSSLMODE=disable outside the config file must not slip past production
// validation.
func TestRequireVerifiedTLSHonoursEnvironment(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	// No sslmode in the DSN itself: the environment supplies it.
	if err := RequireVerifiedTLS("postgres://user@db.mail.invalid/mail"); err == nil {
		t.Fatal("PGSSLMODE=disable must be rejected; got nil")
	}

	if err := os.Setenv("PGSSLMODE", "verify-full"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	if err := RequireVerifiedTLS("postgres://user@db.mail.invalid/mail"); err != nil {
		t.Fatalf("PGSSLMODE=verify-full must be accepted; got %v", err)
	}
}
