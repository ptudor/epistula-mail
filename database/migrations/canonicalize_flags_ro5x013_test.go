package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestCanonicalizeFlagsBackfill is the RO5X-013 data-migration verification.
//
// Migration 011 rewrites the case of system flags already in messages.flags.
// The test seeds rows the way a pre-R-063 server would have written them
// (whatever case the client sent), applies the migration, and asserts every
// system flag came out canonical while keyword flags were left alone.
//
// Applying all migrations first and then re-running 011 is equivalent for this
// purpose and much simpler than staging a partial apply: the migration is a
// plain UPDATE with no dependency on rows existing when it first ran, and
// proving it is safe to run twice is itself worth asserting.
func TestCanonicalizeFlagsBackfill(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := migrations.Apply(ctx, db.Pool()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// Minimal fixture.
	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('flagbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 1) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}

	cases := []struct {
		uid   int64
		seed  []string
		want  []string
		label string
	}{
		{1, []string{`\SEEN`}, []string{`\Seen`}, "uppercase system flag"},
		{2, []string{`\seen`, `\ANSWERED`}, []string{`\Seen`, `\Answered`}, "lowercase + uppercase"},
		{3, []string{`\Seen`}, []string{`\Seen`}, "already canonical"},
		{4, []string{`$Forwarded`, `\DELETED`}, []string{`$Forwarded`, `\Deleted`}, "keyword preserved beside a system flag"},
		{5, []string{`$JUNK`, `$Junk`, `NonJunk`}, []string{`$JUNK`, `$Junk`, `NonJunk`}, "keywords are case-sensitive, untouched"},
		{6, []string{}, []string{}, "empty"},
		{7, []string{`\FlAgGeD`, `\draft`}, []string{`\Flagged`, `\Draft`}, "mixed case"},
		{8, []string{`\Recent`}, []string{`\Recent`}, "not a system flag despite the backslash"},
	}

	for _, tc := range cases {
		sha := make([]byte, 32)
		sha[0] = byte(tc.uid)
		when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body,
				bodystructure, flags
			) VALUES ($1,$2,$3,$4,10,$5,'s','a@f.invalid','{}','{}','{}','b','{}',$6)`,
			folderID, tc.uid, sha, when, when, tc.seed,
		); err != nil {
			t.Fatalf("insert uid %d: %v", tc.uid, err)
		}
	}

	// Re-run migration 011 against the seeded rows.
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var sql string
	for _, m := range all {
		if m.Version == 11 {
			sql = m.SQL
		}
	}
	if sql == "" {
		t.Fatal("migration 011 not found")
	}
	if _, err := db.Pool().Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 011: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var got []string
			if err := db.Pool().QueryRow(ctx,
				`SELECT flags FROM messages WHERE folder_id = $1 AND uid = $2`,
				folderID, tc.uid,
			).Scan(&got); err != nil {
				t.Fatalf("read flags: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("flags = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("flags = %v, want %v (element %d differs; order must be preserved)",
						got, tc.want, i)
				}
			}
		})
	}

	// Idempotent: a second run changes nothing.
	tag, err := db.Pool().Exec(ctx, sql)
	if err != nil {
		t.Fatalf("re-run migration 011: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Errorf("second run updated %d rows; the migration must be idempotent", n)
	}
}

// TestCanonicalizeFlagsTouchesOnlyAffectedRows pins the WHERE clause: on a
// store that has only ever been written by a post-R-063 server, the migration
// must update nothing at all.
func TestCanonicalizeFlagsTouchesOnlyAffectedRows(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := migrations.Apply(ctx, db.Pool()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('clean', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 1) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}

	for uid, flags := range map[int64][]string{
		1: {`\Seen`},
		2: {`\Seen`, `\Answered`},
		3: {`$Forwarded`},
		4: {},
	} {
		sha := make([]byte, 32)
		sha[0] = byte(uid)
		when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body,
				bodystructure, flags
			) VALUES ($1,$2,$3,$4,10,$5,'s','a@c.invalid','{}','{}','{}','b','{}',$6)`,
			folderID, uid, sha, when, when, flags,
		); err != nil {
			t.Fatalf("insert uid %d: %v", uid, err)
		}
	}

	all, _ := migrations.All()
	var sql string
	for _, m := range all {
		if m.Version == 11 {
			sql = m.SQL
		}
	}
	tag, err := db.Pool().Exec(ctx, sql)
	if err != nil {
		t.Fatalf("run migration 011: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Errorf("updated %d rows on an already-canonical store; the WHERE clause "+
			"must limit the blast radius to rows that need it", n)
	}
}
