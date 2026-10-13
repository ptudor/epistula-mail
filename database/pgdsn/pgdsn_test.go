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
			// RA6X-029's example as reported. "%64isable" is "disable"
			// percent-escaped, so the old negative substring test never fired,
			// and the decoy application_name satisfied the positive one. pgx
			// 5.11 parses URIs exactly as libpq does, and libpq refuses a raw
			// "=" inside a query value, so this DSN no longer connects at all.
			// Production must still refuse it: whatever the connection layer
			// cannot parse, it cannot use.
			name:         "RA6X-029 as reported: a raw = in a query value does not parse",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=%64isable&application_name=sslmode=verify-full",
			wantErr:      true,
			wantContains: "not usable by the connection layer",
		},
		{
			// The same bypass in a form the connection layer accepts: libpq
			// allows a raw "=" in the userinfo, so the decoy moves to the
			// password. The DSN text still contains "sslmode=verify-full" and
			// never "sslmode=disable", and pgx resolves it to plaintext.
			name:         "percent-escaped disable hidden behind a decoy password",
			dsn:          "postgres://user:sslmode=verify-full@db.mail.invalid/mail?sslmode=%64isable",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// A decoy application_name the connection layer accepts must be
			// percent-encoded; it decodes to "sslmode=verify-full" and changes
			// nothing about the effective mode.
			name:         "percent-escaped disable with an encoded decoy application_name",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=%64isable&application_name=sslmode%3Dverify-full",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			// Default mode is `prefer`, which carries a plaintext fallback.
			// The decoy satisfied the old positive test.
			name:         "default prefer with a decoy password",
			dsn:          "postgres://user:sslmode=verify-full@db.mail.invalid/mail",
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
			// The keyword spelling of RA6X-029: a quoted decoy, and "disable"
			// hidden by a backslash escape inside a quoted value.
			name:         "keyword DSN with a decoy application_name and an escaped disable",
			dsn:          `host=db.mail.invalid user=app dbname=mail application_name='sslmode=verify-full' sslmode='d\isable'`,
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
			// connection layer resolves it, whichever way that is. Since 5.11
			// pgx takes the LAST occurrence, as libpq does; before that its
			// net/url-based parser took the first, and this table pinned the
			// opposite answers. These DSNs must be judged by their last
			// sslmode, not by whether "verify-full" appears anywhere.
			name:         "repeated sslmode, the last wins and it is insecure",
			dsn:          "postgres://user@db.mail.invalid/mail?sslmode=verify-full&sslmode=disable",
			wantErr:      true,
			wantContains: "unencrypted",
		},
		{
			name:    "repeated sslmode, the last wins and it is secure",
			dsn:     "postgres://user@db.mail.invalid/mail?sslmode=disable&sslmode=verify-full",
			wantErr: false,
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
