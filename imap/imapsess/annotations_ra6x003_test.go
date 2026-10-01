package imapsess

import (
	"context"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestMoveCarriesAnnotationSidecars is the RA6X-003 regression.
//
// message_annotations is keyed (message_id, model) and cascades on delete, and
// a MOVE creates a new message id and deletes the old one. Filing a message
// therefore destroyed every summary, category, tag, model attribution and
// token count the annotation worker had produced — silently, and with no way
// to recover the association afterwards, because the worker keys on the id.
func TestMoveCarriesAnnotationSidecars(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcID := messageIDOf(t, ctx, sess, sess.selectedFolderID, 1)
	seedAnnotation(t, ctx, sess, srcID, "model-a", []string{"invoice", "urgent"}, "finance", "an invoice", 11, 22)
	seedAnnotation(t, ctx, sess, srcID, "model-b", []string{"bill"}, "other", "a bill", 33, 44)
	wantA := readAnnotation(t, ctx, sess, srcID, "model-a")
	wantB := readAnnotation(t, ctx, sess, srcID, "model-b")

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	destFolder, err := folderIDByName(sess, "Archive/2026")
	if err != nil {
		t.Fatalf("lookup destination: %v", err)
	}
	destID := firstMessageIDIn(t, ctx, sess, destFolder)

	for model, want := range map[string]annotationRow{"model-a": wantA, "model-b": wantB} {
		got := readAnnotation(t, ctx, sess, destID, model)
		if got != want {
			t.Errorf("annotation %s\n got: %+v\nwant: %+v", model, got, want)
		}
	}

	// The source's rows are gone with the source, as the cascade intends.
	if n := annotationCount(t, ctx, sess, srcID); n != 0 {
		t.Errorf("%d annotation(s) survived on the deleted source id", n)
	}
}

// TestCopyCarriesAnnotationSidecars pins the COPY half: a duplicate that
// arrives unannotated is work the annotation worker has to redo for content it
// has already read.
func TestCopyCarriesAnnotationSidecars(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcID := messageIDOf(t, ctx, sess, sess.selectedFolderID, 2)
	seedAnnotation(t, ctx, sess, srcID, "model-a", []string{"receipt"}, "finance", "a receipt", 7, 8)
	want := readAnnotation(t, ctx, sess, srcID, "model-a")

	if _, err := sess.Copy(imap.UIDSetNum(2), "Archive/2026"); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	destFolder, err := folderIDByName(sess, "Archive/2026")
	if err != nil {
		t.Fatalf("lookup destination: %v", err)
	}
	destID := firstMessageIDIn(t, ctx, sess, destFolder)

	if got := readAnnotation(t, ctx, sess, destID, "model-a"); got != want {
		t.Errorf("copied annotation\n got: %+v\nwant: %+v", got, want)
	}
	// The source keeps its own.
	if got := readAnnotation(t, ctx, sess, srcID, "model-a"); got != want {
		t.Errorf("source annotation was disturbed by the copy: %+v", got)
	}
}

// TestUnannotatedMoveIsUnaffected pins that a message with no sidecar moves
// exactly as before — the copy is a join, not a requirement.
func TestUnannotatedMoveIsUnaffected(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	destFolder, err := folderIDByName(sess, "Archive/2026")
	if err != nil {
		t.Fatalf("lookup destination: %v", err)
	}
	destID := firstMessageIDIn(t, ctx, sess, destFolder)
	if n := annotationCount(t, ctx, sess, destID); n != 0 {
		t.Fatalf("an unannotated move produced %d annotation(s)", n)
	}
}

// TestRolledBackMoveLeavesAnnotationsIntact pins that a failed MOVE does not
// disturb the source's sidecars.
func TestRolledBackMoveLeavesAnnotationsIntact(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srcID := messageIDOf(t, ctx, sess, sess.selectedFolderID, 1)
	seedAnnotation(t, ctx, sess, srcID, "model-a", []string{"keep"}, "other", "keep me", 1, 2)
	want := readAnnotation(t, ctx, sess, srcID, "model-a")

	// A MOVE that cannot succeed: the mailbox is over quota is not enough
	// (MOVE is quota-neutral), so remove the source blob instead, which fails
	// the blob-existence check inside the transaction.
	removeRawBlob(t, sess, 1, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive/2026"); err == nil {
		t.Fatal("MOVE with a missing source blob should fail")
	}
	if got := readAnnotation(t, ctx, sess, srcID, "model-a"); got != want {
		t.Fatalf("a rolled-back MOVE changed the source annotation:\n got: %+v\nwant: %+v", got, want)
	}
}

// --- helpers ---------------------------------------------------------------

type annotationRow struct {
	Model     string
	Tags      string
	Category  string
	Summary   string
	TokensIn  int64
	TokensOut int64
	CreatedAt time.Time
}

func seedAnnotation(t *testing.T, ctx context.Context, sess *Session, messageID int64,
	model string, tags []string, category, summary string, tin, tout int64,
) {
	t.Helper()
	if _, err := sess.be.Pool.Exec(ctx, `
		INSERT INTO message_annotations
			(message_id, model, tags, category, summary, tokens_in, tokens_out, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() - interval '1 day')`,
		messageID, model, tags, category, summary, tin, tout,
	); err != nil {
		t.Fatalf("seed annotation %s: %v", model, err)
	}
}

func readAnnotation(t *testing.T, ctx context.Context, sess *Session, messageID int64, model string) annotationRow {
	t.Helper()
	var r annotationRow
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT model, tags::text, COALESCE(category, ''), COALESCE(summary, ''),
		       COALESCE(tokens_in, 0), COALESCE(tokens_out, 0), created_at
		  FROM message_annotations WHERE message_id = $1 AND model = $2`,
		messageID, model,
	).Scan(&r.Model, &r.Tags, &r.Category, &r.Summary, &r.TokensIn, &r.TokensOut, &r.CreatedAt); err != nil {
		t.Fatalf("read annotation %s on %d: %v", model, messageID, err)
	}
	return r
}

func annotationCount(t *testing.T, ctx context.Context, sess *Session, messageID int64) int64 {
	t.Helper()
	var n int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM message_annotations WHERE message_id = $1`, messageID).Scan(&n); err != nil {
		t.Fatalf("count annotations: %v", err)
	}
	return n
}

func messageIDOf(t *testing.T, ctx context.Context, sess *Session, folderID, uid int64) int64 {
	t.Helper()
	var id int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM messages WHERE folder_id = $1 AND uid = $2`, folderID, uid).Scan(&id); err != nil {
		t.Fatalf("message id for uid %d: %v", uid, err)
	}
	return id
}

func firstMessageIDIn(t *testing.T, ctx context.Context, sess *Session, folderID int64) int64 {
	t.Helper()
	var id int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM messages WHERE folder_id = $1 ORDER BY uid LIMIT 1`, folderID).Scan(&id); err != nil {
		t.Fatalf("first message in folder %d: %v", folderID, err)
	}
	return id
}

// removeRawBlob deletes a fixture message's raw blob so COPY/MOVE fails its
// blob-existence check inside the transaction, giving a clean rollback.
func removeRawBlob(t *testing.T, sess *Session, uid int64, when time.Time) {
	t.Helper()
	sha := fixtureSHA(uid)
	p, err := sess.be.BlobStore.PathFor(blob.KindRaw, sess.tenant,
		blob.BucketFromTime(when), hex.EncodeToString(sha))
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
}
