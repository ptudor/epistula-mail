package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestMoveCarriesClassification: archiving from INBOX is a client MOVE into
// the \Archive folder, and the live sorter files the message by its
// classification. MOVE creates a new message id, so without the carry every
// archived message would arrive unclassified and sit unsorted until the
// worker classified the same content again (ARCHIVE_SORTING.md).
func TestMoveCarriesClassification(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcID := messageIDOf(t, ctx, sess, sess.selectedFolderID, 1)
	seedClassification(t, ctx, sess, srcID, "finance/banking", 0.875)
	want := readClassification(t, ctx, sess, srcID)

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	destFolder, err := folderIDByName(sess, "Archive")
	if err != nil {
		t.Fatalf("lookup destination: %v", err)
	}
	destID := firstMessageIDIn(t, ctx, sess, destFolder)
	if got := readClassification(t, ctx, sess, destID); got != want {
		t.Fatalf("classification after MOVE\n got: %+v\nwant: %+v", got, want)
	}
	if n := classificationCount(t, ctx, sess, srcID); n != 0 {
		t.Fatalf("%d classification row(s) survived on the deleted source id", n)
	}

	// The move reset the time the row entered its folder, which is what the
	// sorter's settle delay is measured from.
	var fresh bool
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT created_at > now() - interval '1 minute' FROM messages WHERE id = $1`, destID,
	).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("the moved row's created_at is not the time it entered the folder")
	}
}

// TestCopyCarriesClassification pins the COPY half and that an unclassified
// message copies without inventing one.
func TestCopyCarriesClassification(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcID := messageIDOf(t, ctx, sess, sess.selectedFolderID, 2)
	seedClassification(t, ctx, sess, srcID, "travel", 0.5)
	want := readClassification(t, ctx, sess, srcID)
	if _, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/Copied"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	destFolder, err := folderIDByName(sess, "Archive/Copied")
	if err != nil {
		t.Fatalf("lookup destination: %v", err)
	}
	var withRow, total int64
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT count(c.message_id), count(*)
		  FROM messages m LEFT JOIN message_classifications c ON c.message_id = m.id
		 WHERE m.folder_id = $1`, destFolder,
	).Scan(&withRow, &total); err != nil {
		t.Fatal(err)
	}
	if total != 2 || withRow != 1 {
		t.Fatalf("copied %d message(s), %d classified; want 2 and 1", total, withRow)
	}
	var destID int64
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT m.id FROM messages m JOIN message_classifications c ON c.message_id = m.id
		 WHERE m.folder_id = $1`, destFolder,
	).Scan(&destID); err != nil {
		t.Fatal(err)
	}
	if got := readClassification(t, ctx, sess, destID); got != want {
		t.Fatalf("copied classification\n got: %+v\nwant: %+v", got, want)
	}
	if got := readClassification(t, ctx, sess, srcID); got != want {
		t.Fatalf("the source's classification was disturbed: %+v", got)
	}
}

type classificationRow struct {
	Category   string
	Confidence float32
	Model      string
	CreatedAt  time.Time
}

func seedClassification(t *testing.T, ctx context.Context, sess *Session, messageID int64, category string, confidence float32) {
	t.Helper()
	if _, err := sess.be.Pool.Exec(ctx, `
		INSERT INTO message_classifications (message_id, category, confidence, model, created_at)
		VALUES ($1, $2, $3, 'test-model', now() - interval '1 day')`,
		messageID, category, confidence,
	); err != nil {
		t.Fatalf("seed classification: %v", err)
	}
}

func readClassification(t *testing.T, ctx context.Context, sess *Session, messageID int64) classificationRow {
	t.Helper()
	var r classificationRow
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT category, confidence, model, created_at FROM message_classifications WHERE message_id = $1`,
		messageID,
	).Scan(&r.Category, &r.Confidence, &r.Model, &r.CreatedAt); err != nil {
		t.Fatalf("read classification of %d: %v", messageID, err)
	}
	return r
}

func classificationCount(t *testing.T, ctx context.Context, sess *Session, messageID int64) int64 {
	t.Helper()
	var n int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM message_classifications WHERE message_id = $1`, messageID).Scan(&n); err != nil {
		t.Fatalf("count classifications: %v", err)
	}
	return n
}
