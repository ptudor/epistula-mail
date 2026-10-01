package imapsess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestCopyHoldsItsSourceAcrossAttachmentCopy is the RA6X-002 regression, driven
// by a barrier: pause after the destination message INSERT, delete
// the source from another connection, then resume the attachment copy.
//
// The source rows used to be read by three separate READ COMMITTED statements —
// the resolve, the destination INSERT ... SELECT, and the attachment INSERT
// that re-joins `messages`. A concurrent EXPUNGE committing between the second
// and the third made the attachment INSERT match zero rows, so COPY returned
// success with a destination message that had lost its attachments entirely.
//
// The source set is now pinned FOR UPDATE for the whole transaction, so the
// competing delete blocks until COPY commits.
func TestCopyHoldsItsSourceAcrossAttachmentCopy(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Give uid 1 an attachment, so "the attachments came across" is checkable.
	if _, err := sess.be.Pool.Exec(ctx, `
		INSERT INTO attachments (message_id, part_number, filename, content_type,
		                         size_bytes, sha256, blob_date)
		SELECT id, '2', 'report.txt', 'text/plain', 11,
		       $2, raw_blob_date
		  FROM messages WHERE folder_id = $1 AND uid = 1`,
		sess.selectedFolderID, sourceAttachmentSHA(),
	); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	// COPY verifies each referenced blob still exists on disk before creating
	// a new reference to it (R-061), so the attachment needs a real file.
	seedAttachmentBlobFile(t, sess, hex.EncodeToString(sourceAttachmentSHA()),
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	// In the window between the destination INSERT and the attachment copy, a
	// different connection tries to delete the source.
	deleteDone := make(chan error, 1)
	fired := false
	sess.copyRaceHook = func() {
		fired = true
		go func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dcancel()
			_, err := sess.be.Pool.Exec(dctx,
				`DELETE FROM messages WHERE folder_id = $1 AND uid = 1`, sess.selectedFolderID)
			deleteDone <- err
		}()
		// Give the delete a chance to reach the row and block on the lock. If
		// it did NOT block, it commits here and the attachment copy below finds
		// nothing — which is precisely the defect.
		select {
		case err := <-deleteDone:
			t.Errorf("the competing delete completed inside the copy window "+
				"(err=%v); the source was not held", err)
		case <-time.After(500 * time.Millisecond):
			// Blocked, as required.
		}
	}

	data, err := sess.Copy(imap.UIDSetNum(1), "Archive/2026")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if !fired {
		t.Fatal("copy race hook never fired — the test did not exercise the window")
	}

	// The blocked delete may now proceed.
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("the competing delete failed after the copy committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the competing delete never completed after the copy committed")
	}

	// The destination message must have its attachment. A successful COPY that
	// silently dropped it is the defect.
	destUIDs, _ := data.DestUIDs.Nums()
	if len(destUIDs) != 1 {
		t.Fatalf("COPYUID returned %d destination uids, want 1", len(destUIDs))
	}
	var attCount int64
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM attachments a
		  JOIN messages m ON m.id = a.message_id
		  JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1 AND f.name = 'Archive/2026' AND m.uid = $2`,
		sess.mailboxID, int64(destUIDs[0]),
	).Scan(&attCount); err != nil {
		t.Fatalf("count destination attachments: %v", err)
	}
	if attCount != 1 {
		t.Fatalf("destination message has %d attachment(s), want 1 — COPY succeeded while losing them", attCount)
	}
}

// TestMoveKeepsQuotaExactAgainstAConcurrentExpunge pins the MOVE half of
// RA6X-002: MOVE handed copyInTx a pre-read byte count and then ran a DELETE
// whose affected-row count it ignored, so a concurrent expunge of the same rows
// left the mailbox counter too low.
func TestMoveKeepsQuotaExactAgainstAConcurrentExpunge(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	before := usedBytes(t, sess)

	deleteDone := make(chan error, 1)
	sess.copyRaceHook = func() {
		go func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dcancel()
			_, err := sess.be.Pool.Exec(dctx,
				`DELETE FROM messages WHERE folder_id = $1 AND uid = 2`, sess.selectedFolderID)
			deleteDone <- err
		}()
		time.Sleep(300 * time.Millisecond)
	}

	if err := sess.Move(nil, imap.UIDSetNum(2), "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	select {
	case <-deleteDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the competing delete never completed")
	}

	// Whatever the interleaving, the counter must equal the real total.
	if got, want := usedBytes(t, sess), sumRawSize(t, ctx, sess); got != want {
		t.Fatalf("used_bytes = %d but SUM(messages.raw_size) = %d after MOVE raced a delete", got, want)
	}
	if before <= 0 {
		t.Fatalf("fixture used_bytes was %d", before)
	}
}

// seedAttachmentBlobFile places an attachment blob on disk at its canonical
// path, so COPY's blob-existence check passes.
func seedAttachmentBlobFile(t *testing.T, sess *Session, shaHex string, when time.Time) {
	t.Helper()
	p, err := sess.be.BlobStore.PathFor(blob.KindAttachment, sess.tenant, blob.BucketFromTime(when), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir attachment blob dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("attachment\n"), 0o640); err != nil {
		t.Fatalf("write attachment blob: %v", err)
	}
}

func sourceAttachmentSHA() []byte { sum := sha256.Sum256([]byte("attachment\n")); return sum[:] }
