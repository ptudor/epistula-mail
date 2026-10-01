package migrations_test

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestMigration021RecoversMergeRedirects checks the backfill: merges made
// before folder_redirects existed are read back from their archive_moves
// journal. Every source folder a live merge batch moved a message out of
// becomes an exact-name redirect; undone merges, and the other journal
// batches (reorg-, sort-, refile-), do not.
func TestMigration021RecoversMergeRedirects(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := db.Pool()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, m := range all {
		if m.Version >= 21 {
			break
		}
		if _, err := pool.Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_versions (version, description) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			m.Version, m.Description,
		); err != nil {
			t.Fatalf("record migration %d: %v", m.Version, err)
		}
	}

	var mailboxID, folderID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('jdoe', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Sent Messages', 1, 1) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}
	messages := make([]int64, 6)
	for i := range messages {
		sha := make([]byte, 32)
		sha[0] = byte(i + 1)
		when := time.Date(2026, 9, 25, 4, 51, 0, 0, time.UTC)
		if err := pool.QueryRow(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure
			) VALUES ($1,$2,$3,$4,10,$5,'s','a@f.invalid','{}','{}','{}','b','{}') RETURNING id`,
			folderID, i+1, sha, when, when,
		).Scan(&messages[i]); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	journal := []struct {
		message     int64
		batch, from string
		to          string
		undone      bool
	}{
		{messages[0], "merge-20260925T045142Z", "Sent", "Sent Messages", false},
		{messages[1], "merge-20260925T045142Z", "Sent/2008to2015", "Sent Messages", false},
		{messages[2], "merge-20260925T045142Z", "Sent/2008to2015", "Sent Messages", false},
		{messages[3], "merge-20260925T045627Z", "Sent/2008to2015", "Sent Messages", false},
		{messages[4], "merge-20260926T010000Z", "Drafts/old", "Drafts", true},
		{messages[5], "reorg-20260926T020000Z", "webmail-archive/2013-inbox", "Archive/news/other", false},
	}
	for _, j := range journal {
		if _, err := pool.Exec(ctx, `
			INSERT INTO archive_moves (mailbox_id, message_id, batch, reason, from_folder, to_folder, undone_at)
			VALUES ($1, $2, $3, 'reorg', $4, $5, CASE WHEN $6 THEN now() END)`,
			mailboxID, j.message, j.batch, j.from, j.to, j.undone,
		); err != nil {
			t.Fatalf("journal %+v: %v", j, err)
		}
	}

	applied, err := migrations.Apply(ctx, pool)
	if err != nil {
		t.Fatalf("apply migration 021: %v", err)
	}
	if len(applied) == 0 || applied[0].Version != 21 {
		t.Fatalf("applied %d migration(s); want 021 first", len(applied))
	}

	rows, err := pool.Query(ctx, `
		SELECT from_folder || ' -> ' || to_folder || ' [' || batch || ']', subtree
		  FROM folder_redirects WHERE mailbox_id = $1`, mailboxID)
	if err != nil {
		t.Fatalf("read redirects: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var r string
		var subtree bool
		if err := rows.Scan(&r, &subtree); err != nil {
			t.Fatalf("scan redirect: %v", err)
		}
		if subtree {
			t.Errorf("%s was recovered as a subtree redirect; the journal cannot say that", r)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read redirects: %v", err)
	}
	sort.Strings(got)
	want := []string{
		"Sent -> Sent Messages [merge-20260925T045142Z]",
		"Sent/2008to2015 -> Sent Messages [merge-20260925T045142Z]",
		"Sent/2008to2015 -> Sent Messages [merge-20260925T045627Z]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered redirects:\n  %v\nwant:\n  %v", got, want)
	}

	target, redirected, err := db.ResolveImportFolder(ctx, mailboxID, "Sent/2008to2015")
	if err != nil || target != "Sent Messages" || !redirected {
		t.Fatalf("ResolveImportFolder(Sent/2008to2015) = %q, %v, %v; want Sent Messages", target, redirected, err)
	}
}
