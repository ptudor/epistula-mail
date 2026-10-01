package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestCopyAndMoveCarryPassMarker: a message the annotation worker has not
// finished is queued in annotation_pass_required (migration 022). COPY and
// MOVE create new message ids, so without the carry a message a user filed
// before the worker reached it would drop out of the worker's fast round and
// wait for its complete sweep. A finished message has no marker and must not
// gain one.
func TestCopyAndMoveCarryPassMarker(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pending := messageIDOf(t, ctx, sess, sess.selectedFolderID, 1)
	finished := messageIDOf(t, ctx, sess, sess.selectedFolderID, 2)
	markedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := sess.be.Pool.Exec(ctx, `DELETE FROM annotation_pass_required`); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.be.Pool.Exec(ctx,
		`INSERT INTO annotation_pass_required (message_id, marked_at) VALUES ($1, $2)`, pending, markedAt,
	); err != nil {
		t.Fatal(err)
	}
	markerOf := func(folder string) (int64, time.Time) {
		t.Helper()
		folderID, err := folderIDByName(sess, folder)
		if err != nil {
			t.Fatalf("lookup %s: %v", folder, err)
		}
		var n int64
		var at time.Time
		if err := sess.be.Pool.QueryRow(ctx, `
			SELECT count(*), coalesce(max(p.marked_at), 'epoch')
			  FROM messages m JOIN annotation_pass_required p ON p.message_id = m.id
			 WHERE m.folder_id = $1`, folderID,
		).Scan(&n, &at); err != nil {
			t.Fatal(err)
		}
		return n, at
	}

	if _, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/Copied"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n, at := markerOf("Archive/Copied"); n != 1 || !at.Equal(markedAt) {
		t.Fatalf("after COPY the destination has %d marker(s) marked %v; want the pending message's one, marked %v", n, at, markedAt)
	}

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if n, at := markerOf("Archive"); n != 1 || !at.Equal(markedAt) {
		t.Fatalf("after MOVE the destination has %d marker(s) marked %v; want 1, marked %v", n, at, markedAt)
	}
	var left int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM annotation_pass_required WHERE message_id IN ($1, $2)`, pending, finished,
	).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d marker(s) left on the moved source or the finished message; want 0", left)
	}
}
