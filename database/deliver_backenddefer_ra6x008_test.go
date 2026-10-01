package main

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// pgxConnect is a thin alias so the helper below reads clearly.
var pgxConnect = pgx.Connect

// TestDeliverDefersBackendFailures is the RA6X-008 regression: an
// operator-correctable backend fault (unapplied migration, revoked GRANT,
// schema drift) must DEFER queued mail, never bounce it.
//
// Before the fix these paths ran storage.IsRetryable, which correctly answers
// "a replay of this SQL fails identically" for SQLSTATE 42/22/23 and so
// returned EX_SOFTWARE — a 5.x.x bounce to Postfix. The message was destroyed
// because a table was missing or a grant had been revoked.
func TestDeliverDefersBackendFailures(t *testing.T) {
	cases := []struct {
		name   string
		damage string // SQL that breaks the backend the way an operator would
	}{
		{
			// An unapplied migration / partially restored dump: the
			// resolver's first table is not there at all (SQLSTATE 42P01).
			name:   "missing required table",
			damage: `DROP TABLE aliases, domain_acl, domains CASCADE`,
		},
		{
			// Schema drift: a column the resolver selects has been renamed
			// out from under it (SQLSTATE 42703).
			name:   "mismatched schema",
			damage: `ALTER TABLE domains RENAME COLUMN is_wildcard TO is_wildcard_old`,
		},
		{
			// A revoked GRANT (SQLSTATE 42501). REVOKE only bites a
			// non-superuser, so the delivery runs as a freshly created role.
			name: "revoked select privilege",
			damage: `REVOKE SELECT ON domains FROM PUBLIC;
			         REVOKE ALL ON ALL TABLES IN SCHEMA public FROM ra6x008_lowpriv`,
		},
		{
			// Schema drift past the resolver: recipient resolution succeeds
			// and the blobs land, then the ingest transaction trips a
			// constraint nobody in the application knows about (SQLSTATE
			// 23514). storage.IsRetryable answers "no" — correctly, a replay
			// fails identically — but it is still the operator's problem, not
			// the sender's.
			name:   "constraint violation from schema drift",
			damage: `ALTER TABLE messages ADD CONSTRAINT ra6x008_drift CHECK (raw_size < 0)`,
		},
		{
			// Revoked INSERT: the resolver reads fine, the ingest write is
			// denied (SQLSTATE 42501) partway down the delivery path.
			name:   "revoked insert privilege",
			damage: `REVOKE INSERT ON messages FROM ra6x008_lowpriv`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dsn, teardown := deliverFixture(t)
			defer teardown()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			damageDatabase(t, ctx, dsn, tc.damage)

			code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid",
				"From: sender@ra6x008.invalid\r\nSubject: keep me queued\r\n\r\nbody\r\n")

			if code != EX_TEMPFAIL {
				t.Fatalf("backend fault returned %d (%s, %s); want EX_TEMPFAIL so Postfix requeues",
					code, ExitCodeName(code), exitDisposition(code))
			}
			if IsPermanentFailure(code) {
				t.Fatalf("backend fault bounced the message (code %d)", code)
			}
			// A deferred delivery must leave nothing half-committed: the
			// retry after repair has to be able to insert the message once.
			if n := countMessageRows(t, ctx, dsn); n != 0 {
				t.Fatalf("deferred delivery committed %d message row(s); want 0", n)
			}
		})
	}
}

// TestDeliverKeepsPermanentPolicyCodes pins the other half of RA6X-008: making
// backend faults defer must not turn genuine per-message or per-recipient
// policy rejections into infinite retries.
func TestDeliverKeepsPermanentPolicyCodes(t *testing.T) {
	cases := []struct {
		name      string
		recipient string
		raw       string
		want      int
	}{
		{
			name:      "unknown domain",
			recipient: "nobody@not-a-domain-here.invalid",
			raw:       "From: s@ra6x008.invalid\r\nSubject: x\r\n\r\nbody\r\n",
			want:      EX_NOHOST,
		},
		{
			name:      "no recipient match",
			recipient: "no-such-user@ra6x008.invalid",
			raw:       "From: s@ra6x008.invalid\r\nSubject: x\r\n\r\nbody\r\n",
			want:      EX_NOUSER,
		},
		{
			name:      "malformed message",
			recipient: "ra6x008@ra6x008.invalid",
			raw:       "not a message at all, no headers, no separator",
			want:      EX_DATAERR,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, teardown := deliverFixture(t)
			defer teardown()

			code := deliverBytes(t, cfg, tc.recipient, "sender@ra6x008.invalid", tc.raw)
			if code != tc.want {
				t.Fatalf("got %d (%s); want %d (%s)",
					code, ExitCodeName(code), tc.want, ExitCodeName(tc.want))
			}
		})
	}
}

// deliverFixture builds a migrated database with one deliverable recipient and
// a Config wired to it, plus a real blob store under t.TempDir().
func deliverFixture(t *testing.T) (*Config, string, func()) {
	t.Helper()
	db, dsn := pgtest.Open(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var domainID, mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO domains (name, is_wildcard) VALUES ('ra6x008.invalid', false) RETURNING id`,
	).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('ra6x008', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, 'ra6x008', $2)`,
		domainID, mailboxID,
	); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	// A non-superuser role so REVOKE actually denies something.
	if _, err := db.Pool().Exec(ctx,
		`DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ra6x008_lowpriv') THEN
		        CREATE ROLE ra6x008_lowpriv LOGIN;
		    END IF;
		 END $$`,
	); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO ra6x008_lowpriv;
		 GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO ra6x008_lowpriv;
		 GRANT USAGE ON SCHEMA public TO ra6x008_lowpriv`,
	); err != nil {
		t.Fatalf("grant: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Postgres.DSN = withUser(t, dsn, "ra6x008_lowpriv")
	cfg.Storage.Root = t.TempDir()
	cfg.Delivery.Timeout = "60s"

	return cfg, dsn, func() {}
}

// deliverBytes runs the real deliver() with raw on stdin and returns its code.
func deliverBytes(t *testing.T, cfg *Config, envTo, envFrom, raw string) int {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "msg")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}

	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved; _ = f.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return deliver(ctx, cfg, &deliveryAcceptance{}, envTo, envFrom)
}

// damageDatabase applies operator-scale breakage to the test database using a
// privileged connection.
func damageDatabase(t *testing.T, ctx context.Context, dsn, sql string) {
	t.Helper()
	conn, err := pgxConnect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for damage: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("apply damage %q: %v", sql, err)
	}
}

// withUser rewrites a URL DSN's userinfo so the delivery runs as a
// non-superuser role.
func withUser(t *testing.T, dsn, user string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.User(user)
	return u.String()
}

// countMessageRows reads the committed message count over a privileged
// connection, so a deferred delivery can be shown to have left no partial row.
func countMessageRows(t *testing.T, ctx context.Context, dsn string) int64 {
	t.Helper()
	conn, err := pgxConnect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for count: %v", err)
	}
	defer conn.Close(ctx)
	var n int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}
