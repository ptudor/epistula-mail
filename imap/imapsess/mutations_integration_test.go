package imapsess

import (
	"context"
	"crypto/sha256"
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

// Fixtures use real content addresses and the same 100 bytes counted by quota.
func fixtureRaw(uid int64) []byte    { return []byte(fmt.Sprintf("%0100d", uid)) }
func fixtureSHA(uid int64) []byte    { sum := sha256.Sum256(fixtureRaw(uid)); return sum[:] }
func fixtureAtt(uid int64) []byte    { return []byte(fmt.Sprintf("%042d", uid)) }
func fixtureAttSHA(uid int64) []byte { sum := sha256.Sum256(fixtureAtt(uid)); return sum[:] }
func seedRawBlobFile(t *testing.T, sess *Session, uid int64, when time.Time) {
	t.Helper()
	p, err := sess.be.BlobStore.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(when), hex.EncodeToString(fixtureSHA(uid)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, fixtureRaw(uid), 0640); err != nil {
		t.Fatal(err)
	}
}

// mutationFixture installs 3 messages and gives back a session.
func mutationFixture(t *testing.T) *Session {
	t.Helper()
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 4) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	hdrJSON, _ := json.Marshal(map[string][]string{})
	bsJSON, _ := json.Marshal(map[string]any{"type": "text", "subtype": "plain", "size": 100})
	for uid, flags := range map[int64][]string{
		1: {},
		2: {`\Flagged`},
		3: {`\Deleted`},
	} {
		sha := fixtureSHA(uid)
		when := time.Date(2026, 5, int(uid), 12, 0, 0, 0, time.UTC)
		if _, err := sess.be.Pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				message_id, subject, from_addr, to_addrs, cc_addrs,
				sent_date, headers, text_body, bodystructure, flags
			) VALUES (
				$1, $2, $3, $4, 100, $5,
				$6, $7, 'sender@x', '{}'::text[], '{}'::text[],
				$5, $8, 'body', $9, $10
			)`,
			folderID, uid, sha, when, when,
			"id-"+string(rune('0'+uid)), "Subject "+string(rune('0'+uid)),
			hdrJSON, bsJSON, flags,
		); err != nil {
			t.Fatalf("insert msg %d: %v", uid, err)
		}
		seedRawBlobFile(t, sess, uid, when)
	}
	// used_bytes after 3 × 100 inserts.
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE mailboxes SET used_bytes = 300 WHERE id = $1`, sess.mailboxID,
	); err != nil {
		t.Fatalf("seed used_bytes: %v", err)
	}
	return sess
}

func messageFlags(t *testing.T, sess *Session, uid int64) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var flags []string
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT flags FROM messages WHERE folder_id = $1 AND uid = $2`,
		sess.selectedFolderID, uid,
	).Scan(&flags); err != nil {
		t.Fatalf("flags lookup for uid %d: %v", uid, err)
	}
	return flags
}

func messageExists(t *testing.T, sess *Session, folderID, uid int64) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE folder_id = $1 AND uid = $2)`,
		folderID, uid,
	).Scan(&exists); err != nil {
		t.Fatalf("exists lookup: %v", err)
	}
	return exists
}

func usedBytes(t *testing.T, sess *Session) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, sess.mailboxID,
	).Scan(&n); err != nil {
		t.Fatalf("used_bytes lookup: %v", err)
	}
	return n
}

func TestStoreAddSeen(t *testing.T) {
	sess := mutationFixture(t)
	flags := &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}
	if err := sess.Store(nil, imap.UIDSetNum(1, 2), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	for _, uid := range []int64{1, 2} {
		f := messageFlags(t, sess, uid)
		seen := false
		for _, x := range f {
			if x == `\Seen` {
				seen = true
			}
		}
		if !seen {
			t.Errorf("uid %d flags = %v, want \\Seen", uid, f)
		}
	}
	// Uid 2 keeps its \Flagged.
	f := messageFlags(t, sess, 2)
	keptFlagged := false
	for _, x := range f {
		if x == `\Flagged` {
			keptFlagged = true
		}
	}
	if !keptFlagged {
		t.Errorf("uid 2 lost \\Flagged: %v", f)
	}
}

func TestStoreRemoveFlag(t *testing.T) {
	sess := mutationFixture(t)
	flags := &imap.StoreFlags{
		Op:     imap.StoreFlagsDel,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagFlagged},
	}
	if err := sess.Store(nil, imap.UIDSetNum(2), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	f := messageFlags(t, sess, 2)
	for _, x := range f {
		if x == `\Flagged` {
			t.Errorf("uid 2 still has \\Flagged after REMOVE: %v", f)
		}
	}
}

func TestStoreSetReplaces(t *testing.T) {
	sess := mutationFixture(t)
	flags := &imap.StoreFlags{
		Op:     imap.StoreFlagsSet,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagAnswered},
	}
	if err := sess.Store(nil, imap.UIDSetNum(2), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	f := messageFlags(t, sess, 2)
	if len(f) != 1 || f[0] != `\Answered` {
		t.Errorf("uid 2 flags = %v, want exactly [\\Answered]", f)
	}
}

func TestExpungeDeletesFlagged(t *testing.T) {
	sess := mutationFixture(t)
	startBytes := usedBytes(t, sess)
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 should be expunged but still present")
	}
	if !messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("uid 1 should remain (not \\Deleted)")
	}
	// 100 bytes returned to the mailbox.
	if want, got := startBytes-100, usedBytes(t, sess); got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}
}

func TestCopyDuplicatesRows(t *testing.T) {
	sess := mutationFixture(t)
	data, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/2026")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	srcs, _ := data.SourceUIDs.Nums()
	dsts, _ := data.DestUIDs.Nums()
	if len(srcs) != 2 || len(dsts) != 2 {
		t.Fatalf("Copy returned src=%v dst=%v, want 2 each", srcs, dsts)
	}

	// Source folder unchanged.
	if !messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("source uid 1 vanished after COPY (should be untouched)")
	}

	// Destination has the new rows.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var destFolderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID,
	).Scan(&destFolderID); err != nil {
		t.Fatalf("dest folder lookup: %v", err)
	}
	for _, uid := range dsts {
		if !messageExists(t, sess, destFolderID, int64(uid)) {
			t.Errorf("dest uid %d missing after COPY", uid)
		}
	}
}

func TestMoveAtomicOneTransaction(t *testing.T) {
	sess := mutationFixture(t)
	startBytes := usedBytes(t, sess)

	// Move uids 1, 2 to a new folder. nil writer is fine — protocol
	// responses are after-commit and gracefully no-op when w is nil.
	if err := sess.Move(nil, imap.UIDSetNum(1, 2), "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Sources are gone in one shot.
	if messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("source uid 1 should be gone after MOVE")
	}
	if messageExists(t, sess, sess.selectedFolderID, 2) {
		t.Error("source uid 2 should be gone after MOVE")
	}
	// uid 3 (\Deleted but not moved) remains.
	if !messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 should remain (not part of the MOVE)")
	}

	// Dest folder exists and has 2 new rows.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var destFolderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID,
	).Scan(&destFolderID); err != nil {
		t.Fatalf("dest folder lookup: %v", err)
	}
	var destCount int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1`, destFolderID,
	).Scan(&destCount); err != nil {
		t.Fatalf("count dest: %v", err)
	}
	if destCount != 2 {
		t.Errorf("dest folder has %d messages, want 2", destCount)
	}

	// used_bytes: -200 (deleted source) +200 (copied dest) = no net change.
	if want, got := startBytes, usedBytes(t, sess); got != want {
		t.Errorf("used_bytes = %d, want %d (MOVE is net-zero on quota)", got, want)
	}
}

func TestMoveCopiesAndDeletes(t *testing.T) {
	sess := mutationFixture(t)
	startBytes := usedBytes(t, sess)
	// Exercises the data side of a move as separate commands — COPY, then
	// STORE \Deleted and EXPUNGE on the source — the sequence a client
	// without MOVE uses. TestMoveAtomicOneTransaction covers Move itself.
	data, err := sess.Copy(imap.UIDSetNum(1, 2), "MovedHere")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if data == nil {
		t.Fatal("CopyData nil")
	}
	// Now flag-and-expunge the originals to mimic the delete half.
	flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := sess.Store(nil, imap.UIDSetNum(1, 2), flags, nil); err != nil {
		t.Fatalf("Store \\Deleted: %v", err)
	}
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("source uid 1 should be gone after move-equivalent")
	}
	// Quota arithmetic: start 300, copy +200 (2 messages × 100 — note
	// COPY adds quota even when source+dest live under the same
	// mailbox; the schema is per-row, not per-deduped-blob), then
	// expunge -300 (uids 1, 2, 3). Final: 200.
	if want, got := startBytes+200-300, usedBytes(t, sess); got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}
}

func TestUIDExpungeStarRangeResolves(t *testing.T) {
	sess := mutationFixture(t)
	// UID 3 carries \Deleted. "3:*" must resolve the star against the
	// folder's highest UID and expunge it — not silently no-op.
	uids := imap.UIDSet{{Start: 3, Stop: 0}}
	if err := sess.Expunge(nil, &uids); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 still present; UID EXPUNGE 3:* was dropped")
	}
	if got := usedBytes(t, sess); got != 200 {
		t.Errorf("used_bytes = %d, want 200 after expunging one 100-byte message", got)
	}
}

func TestUIDExpungeHugeRangeStaysBounded(t *testing.T) {
	sess := mutationFixture(t)
	// A hostile full-uint32 range must not be enumerated into memory; it
	// should behave exactly like "everything deleted in range".
	uids := imap.UIDSet{{Start: 1, Stop: 4_000_000_000}}
	if err := sess.Expunge(nil, &uids); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 still present; ranged UID EXPUNGE missed it")
	}
	// Non-deleted messages survive.
	if !messageExists(t, sess, sess.selectedFolderID, 1) || !messageExists(t, sess, sess.selectedFolderID, 2) {
		t.Error("non-\\Deleted messages must survive UID EXPUNGE")
	}
}
