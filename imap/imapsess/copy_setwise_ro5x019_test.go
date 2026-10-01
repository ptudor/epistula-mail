package imapsess

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
)

// seedForCopy inserts n messages with one attachment each, so the set-wise
// COPY's attachment join is exercised too.
func seedForCopy(t *testing.T, sess *Session, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var nextUID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(uid), 0) + 1 FROM messages WHERE folder_id = $1`,
		sess.selectedFolderID,
	).Scan(&nextUID); err != nil {
		t.Fatalf("next uid: %v", err)
	}

	hdrJSON, _ := json.Marshal(map[string][]string{})
	bsJSON, _ := json.Marshal(map[string]any{"type": "text", "subtype": "plain", "size": 10})
	for i := 0; i < n; i++ {
		uid := nextUID + int64(i)
		// Match the actual content written by seedRawBlobFile.
		sha := fixtureSHA(uid)
		when := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		var msgID int64
		if err := sess.be.Pool.QueryRow(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				message_id, subject, from_addr, to_addrs, cc_addrs,
				sent_date, headers, text_body, bodystructure, flags
			) VALUES ($1,$2,$3,$4,100,$5,$6,'s','x@y.invalid','{}','{}',$5,$7,'b',$8,'{}')
			RETURNING id`,
			sess.selectedFolderID, uid, sha, when, when,
			fmt.Sprintf("<c%d@x.invalid>", i), hdrJSON, bsJSON,
		).Scan(&msgID); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
		if _, err := sess.be.Pool.Exec(ctx, `
			INSERT INTO attachments (
				message_id, part_number, filename, content_type,
				content_id, disposition, size_bytes, sha256, blob_date
			) VALUES ($1,'2',$2,'application/pdf',NULL,'attachment',42,$3,$4)`,
			msgID, fmt.Sprintf("f%d.pdf", i), fixtureAttSHA(uid), when,
		); err != nil {
			t.Fatalf("insert attachment %d: %v", i, err)
		}
		seedRawBlobFile(t, sess, uid, when)
		seedAttBlobFile(t, sess, uid, when)
	}
}

// seedAttBlobFile is seedRawBlobFile's attachment counterpart. lockCopyBlobs
// takes the GC advisory lock for attachment blobs too (R-061), so a COPY of a
// message with attachments needs both files present.
func seedAttBlobFile(t *testing.T, sess *Session, uid int64, when time.Time) {
	t.Helper()
	sha := fixtureAttSHA(uid)
	p, err := sess.be.BlobStore.PathFor(blob.KindAttachment, sess.tenant,
		blob.BucketFromTime(when), hex.EncodeToString(sha))
	if err != nil {
		t.Fatalf("PathFor att: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir att dir: %v", err)
	}
	if err := os.WriteFile(p, fixtureAtt(uid), 0o640); err != nil {
		t.Fatalf("write att blob: %v", err)
	}
}

// TestSetWiseCopyPreservesCopyUIDContract is the RO5X-019 verification.
//
// The per-row loop cost four statements per message and held the destination
// folder's row lock for the whole COPY, blocking every concurrent LDA delivery
// into that folder. The set-wise rewrite must preserve RFC 4315's COPYUID
// contract exactly: source and destination UID sets correspond positionally in
// ascending source-UID order.
func TestSetWiseCopyPreservesCopyUIDContract(t *testing.T) {
	sess := mutationFixture(t)
	const extra = 200
	seedForCopy(t, sess, extra)

	beforeBytes := usedBytes(t, sess)

	data, err := sess.Copy(imap.UIDSetNum(), "Archive/2026")
	if err == nil && data != nil {
		// An empty set is a no-op; the real work is below.
		_ = data
	}

	// Copy everything.
	all := imap.UIDSet{}
	all.AddRange(1, 0) // 1:* — every message in the folder
	data, err = sess.Copy(all, "Archive/2026")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}

	srcs, _ := data.SourceUIDs.Nums()
	dsts, _ := data.DestUIDs.Nums()
	if len(srcs) != len(dsts) {
		t.Fatalf("COPYUID sets differ in length: %d source, %d dest", len(srcs), len(dsts))
	}
	if len(srcs) != extra+3 {
		t.Fatalf("copied %d messages, want %d", len(srcs), extra+3)
	}

	// Source UIDs ascending, destination UIDs ascending and contiguous —
	// positionally corresponding, per RFC 4315.
	for i := 1; i < len(srcs); i++ {
		if srcs[i] <= srcs[i-1] {
			t.Fatalf("source UIDs not ascending at %d: %v", i, srcs[i-1:i+1])
		}
		if dsts[i] != dsts[i-1]+1 {
			t.Fatalf("dest UIDs not contiguous at %d: %d then %d", i, dsts[i-1], dsts[i])
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var destFolderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID).Scan(&destFolderID); err != nil {
		t.Fatalf("dest folder: %v", err)
	}

	// Every (source uid → dest uid) pair must name the same message.
	for i := range srcs {
		var srcMsgID, dstMsgID int64
		var srcSubject, dstSubject *string
		if err := sess.be.Pool.QueryRow(ctx,
			`SELECT id, message_id FROM messages WHERE folder_id = $1 AND uid = $2`,
			sess.selectedFolderID, int64(srcs[i]),
		).Scan(&srcMsgID, &srcSubject); err != nil {
			t.Fatalf("source uid %d: %v", srcs[i], err)
		}
		if err := sess.be.Pool.QueryRow(ctx,
			`SELECT id, message_id FROM messages WHERE folder_id = $1 AND uid = $2`,
			destFolderID, int64(dsts[i]),
		).Scan(&dstMsgID, &dstSubject); err != nil {
			t.Fatalf("dest uid %d: %v", dsts[i], err)
		}
		if (srcSubject == nil) != (dstSubject == nil) ||
			(srcSubject != nil && *srcSubject != *dstSubject) {
			t.Fatalf("COPYUID pair %d mis-maps: source uid %d (message_id %v) "+
				"→ dest uid %d (message_id %v)", i, srcs[i], srcSubject, dsts[i], dstSubject)
		}
	}

	// All attachments landed, one per copied message.
	var attCount int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM attachments a
		   JOIN messages m ON m.id = a.message_id
		  WHERE m.folder_id = $1`, destFolderID,
	).Scan(&attCount); err != nil {
		t.Fatalf("count dest attachments: %v", err)
	}
	if attCount != extra {
		t.Errorf("dest attachments = %d, want %d", attCount, extra)
	}

	// used_bytes moved by the exact sum.
	var wantBytes int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(raw_size),0) FROM messages WHERE folder_id = $1`,
		sess.selectedFolderID).Scan(&wantBytes); err != nil {
		t.Fatalf("sum source bytes: %v", err)
	}
	if got, want := usedBytes(t, sess), beforeBytes+wantBytes; got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}

	// Dest uidnext advanced by exactly the number copied.
	var uidnext int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT uidnext FROM folders WHERE id = $1`, destFolderID).Scan(&uidnext); err != nil {
		t.Fatalf("dest uidnext: %v", err)
	}
	if want := int64(dsts[len(dsts)-1]) + 1; uidnext != want {
		t.Errorf("dest uidnext = %d, want %d", uidnext, want)
	}
}

// TestSetWiseCopyHoldsFolderLockBriefly is the contention test: an LDA
// delivery into the destination folder must not queue behind a large COPY for
// anything like the 60 s delivery watchdog.
func TestSetWiseCopyHoldsFolderLockBriefly(t *testing.T) {
	sess := mutationFixture(t)
	seedForCopy(t, sess, 500)

	// Create the destination up front so the contending statement can target
	// the same row the COPY will lock.
	if err := sess.Create("Archive/2026", nil); err != nil {
		t.Fatalf("Create dest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var destFolderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID).Scan(&destFolderID); err != nil {
		t.Fatalf("dest folder: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		all := imap.UIDSet{}
		all.AddRange(1, 0)
		_, err := sess.Copy(all, "Archive/2026")
		done <- err
	}()

	// Give the COPY a moment to get going, then run the delivery path's
	// UID-allocation statement against the same folder row and time it.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	var uid, modseq int64
	err := sess.be.Pool.QueryRow(ctx,
		`UPDATE folders SET uidnext = uidnext + 1, highest_modseq = highest_modseq + 1
		  WHERE id = $1 RETURNING uidnext - 1, highest_modseq`,
		destFolderID,
	).Scan(&uid, &modseq)
	blocked := time.Since(start)

	if err != nil {
		t.Fatalf("contending delivery-style allocation: %v", err)
	}
	if copyErr := <-done; copyErr != nil {
		t.Fatalf("Copy: %v", copyErr)
	}

	t.Logf("contending UID allocation waited %v behind a 503-message COPY", blocked)
	// The delivery watchdog is 60s. Per-row locking made this scale with the
	// message count; set-wise it should be a small fraction of a second.
	if blocked > 10*time.Second {
		t.Errorf("delivery-style allocation blocked for %v behind the COPY; "+
			"the destination folder row lock is being held too long (RO5X-019)", blocked)
	}
}

// TestSetWiseMoveStillAtomic keeps MOVE's semantics: copy + delete in one tx,
// with the source emptied and the destination complete.
func TestSetWiseMoveStillAtomic(t *testing.T) {
	sess := mutationFixture(t)
	seedForCopy(t, sess, 50)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var srcBefore int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1`, sess.selectedFolderID,
	).Scan(&srcBefore); err != nil {
		t.Fatalf("count source: %v", err)
	}

	all := imap.UIDSet{}
	all.AddRange(1, 0)
	if err := sess.Move(nil, all, "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	var srcAfter, dstAfter int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1`, sess.selectedFolderID,
	).Scan(&srcAfter); err != nil {
		t.Fatalf("count source after: %v", err)
	}
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE f.mailbox_id = $1 AND f.name = 'Archive/2026'`, sess.mailboxID,
	).Scan(&dstAfter); err != nil {
		t.Fatalf("count dest: %v", err)
	}
	if srcAfter != 0 {
		t.Errorf("source folder has %d messages after MOVE, want 0", srcAfter)
	}
	if dstAfter != srcBefore {
		t.Errorf("dest has %d messages, want %d", dstAfter, srcBefore)
	}
}
