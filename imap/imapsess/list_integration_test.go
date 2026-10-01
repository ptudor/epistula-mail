package imapsess

import (
	"context"
	"testing"
	"time"
)

// listForTest runs the same folder query Session.List runs and returns its
// rows. go-imap's ListWriter has no exported constructor, so a test cannot
// call List directly; replicating its SQL here checks the parts these tests
// care about — the SelectSubscribed filter and the subscribed column —
// without poking at upstream internals. Keep the query in step with List.
func listForTest(t *testing.T, sess *Session, selectSubscribed bool) []listRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		SELECT f.name, f.special_use,
		       EXISTS(SELECT 1 FROM folder_subscriptions s
		               WHERE s.mailbox_id = f.mailbox_id AND s.folder_id = f.id) AS subscribed
		  FROM folders f
		 WHERE f.mailbox_id = $1`
	if selectSubscribed {
		query += ` AND EXISTS (SELECT 1 FROM folder_subscriptions s
		                        WHERE s.mailbox_id = f.mailbox_id AND s.folder_id = f.id)`
	}
	query += ` ORDER BY f.name`

	rows, err := sess.be.Pool.Query(ctx, query, sess.mailboxID)
	if err != nil {
		t.Fatalf("list query: %v", err)
	}
	defer rows.Close()
	var out []listRow
	for rows.Next() {
		var r listRow
		if err := rows.Scan(&r.name, &r.specialUse, &r.subscribed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type listRow struct {
	name       string
	specialUse *string
	subscribed bool
}

func TestLSubFiltersToSubscribedFolders(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Three folders, two subscribed.
	type folder struct {
		name      string
		subscribe bool
	}
	folders := []folder{
		{"INBOX", true},
		{"Sent", true},
		{"Archive", false},
	}
	for _, f := range folders {
		var folderID int64
		if err := sess.be.Pool.QueryRow(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			 VALUES ($1, $2, $3, 1) RETURNING id`,
			sess.mailboxID, f.name, time.Now().Unix(),
		).Scan(&folderID); err != nil {
			t.Fatalf("insert folder %q: %v", f.name, err)
		}
		if f.subscribe {
			if _, err := sess.be.Pool.Exec(ctx,
				`INSERT INTO folder_subscriptions (mailbox_id, folder_id)
				 VALUES ($1, $2)`,
				sess.mailboxID, folderID,
			); err != nil {
				t.Fatalf("subscribe %q: %v", f.name, err)
			}
		}
	}

	// LIST returns all three.
	all := listForTest(t, sess, false)
	if len(all) != 3 {
		t.Errorf("LIST returned %d folders, want 3", len(all))
	}

	// LSUB-equivalent (SelectSubscribed=true) returns only INBOX + Sent.
	sub := listForTest(t, sess, true)
	if len(sub) != 2 {
		t.Errorf("LSUB returned %d folders, want 2", len(sub))
	}
	gotNames := map[string]bool{}
	for _, r := range sub {
		gotNames[r.name] = true
	}
	if !gotNames["INBOX"] || !gotNames["Sent"] {
		t.Errorf("LSUB missing expected folders: got %v", gotNames)
	}
	if gotNames["Archive"] {
		t.Error("LSUB returned Archive which is not subscribed")
	}
}

func TestListReportsSubscribedFlag(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Drafts', $2, 1) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	// Before Subscribe(), the subscribed flag is false.
	rows := listForTest(t, sess, false)
	for _, r := range rows {
		if r.name == "Drafts" && r.subscribed {
			t.Error("Drafts should not be marked subscribed yet")
		}
	}

	// After Subscribe, the flag flips on.
	if err := sess.Subscribe("Drafts"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	rows = listForTest(t, sess, false)
	for _, r := range rows {
		if r.name == "Drafts" && !r.subscribed {
			t.Error("Drafts should be marked subscribed after Subscribe()")
		}
	}
}

func TestUnsubscribeRemovesFromLSub(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Trash', $2, 1) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if err := sess.Subscribe("Trash"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if rows := listForTest(t, sess, true); len(rows) != 1 || rows[0].name != "Trash" {
		t.Fatalf("LSUB after Subscribe = %v, want [Trash]", rows)
	}
	if err := sess.Unsubscribe("Trash"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if rows := listForTest(t, sess, true); len(rows) != 0 {
		t.Errorf("LSUB after Unsubscribe = %v, want []", rows)
	}
}
