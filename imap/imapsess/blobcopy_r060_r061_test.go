package imapsess

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestStreamRawBodyTruncatedBlob is the R-060 regression: a blob shorter than
// the DB raw_size must be refused with NO [SERVERBUG] *before* any literal is
// announced, instead of streaming fewer bytes than promised and desyncing the
// client into a hang.
func TestStreamRawBodyTruncatedBlob(t *testing.T) {
	sess, store := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mustCreateFolder(t, sess, "INBOX")
	data, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), &imap.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	sess.selectedFolderID = 0
	var folderID int64
	var shaHex string
	var blobDate time.Time
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT folder_id, encode(raw_sha256,'hex'), raw_blob_date FROM messages WHERE uid = $1`,
		int64(data.UID),
	).Scan(&folderID, &shaHex, &blobDate); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	// Truncate the on-disk blob to one byte short of raw_size.
	path, err := store.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(blobDate), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.Truncate(path, int64(len(appendRawMsg))-1); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	r := fetchRow{
		uid:          int64(data.UID),
		rawSHA256Hex: shaHex,
		rawBlobDate:  blobDate,
		rawSize:      int64(len(appendRawMsg)),
	}
	// nil resp is safe: the size check must return before WriteBodySection.
	err = sess.streamRawBody(nil, r, &imap.FetchItemBodySection{})
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("streamRawBody = %v (%T), want *imap.Error", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo || imapErr.Code != imap.ResponseCodeServerBug {
		t.Errorf("got %v/%v, want NO [SERVERBUG]", imapErr.Type, imapErr.Code)
	}
}

// gcCandidateCount returns how many gc_candidates rows exist for a given raw sha.
func gcCandidateCount(t *testing.T, sess *Session, tenant, shaHex string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM gc_candidates WHERE tenant = $1 AND sha256 = decode($2,'hex') AND kind = 'raw'`,
		tenant, shaHex,
	).Scan(&n); err != nil {
		t.Fatalf("candidate count: %v", err)
	}
	return n
}

// rawShaHex is the fixture's real per-uid content digest.
func rawShaHex(uid int64) string {
	sha := fixtureSHA(uid)
	return hex.EncodeToString(sha)
}

// TestCopyResurrectsGCCandidate is the R-061 regression: COPY takes the per-blob
// advisory lock and drops any gc_candidates row for the source blob, so a sweep
// can't reap a blob a fresh COPY reference points at.
func TestCopyResurrectsGCCandidate(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// uid 1's raw blob: bucket 2026/05/01 (see mutationFixture).
	shaHex := rawShaHex(1)
	if _, err := sess.be.Pool.Exec(ctx,
		`INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		 VALUES ($1, decode($2,'hex'), 'raw', '2026/05/01', now() - interval '2 days', 1)`,
		string(sess.tenant), shaHex,
	); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if n := gcCandidateCount(t, sess, string(sess.tenant), shaHex); n != 1 {
		t.Fatalf("candidate precondition = %d, want 1", n)
	}

	if _, err := sess.Copy(imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n := gcCandidateCount(t, sess, string(sess.tenant), shaHex); n != 0 {
		t.Errorf("COPY did not resurrect the gc candidate: %d rows remain", n)
	}
}

// TestCopyFailsWhenSourceBlobMissing is the R-061 regression: if a sweep already
// unlinked the source blob, COPY must fail cleanly (no committed row pointing at
// an absent file) rather than create a dangling reference.
func TestCopyFailsWhenSourceBlobMissing(t *testing.T) {
	sess := mutationFixture(t)

	// Remove uid 1's on-disk raw blob (simulate a sweep that won the race).
	shaHex := rawShaHex(1)
	path, err := sess.be.BlobStore.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	_, err = sess.Copy(imap.UIDSetNum(1), "Archive/2026")
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("Copy = %v (%T), want *imap.Error", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo || imapErr.Code != imap.ResponseCodeServerBug {
		t.Errorf("got %v/%v, want NO [SERVERBUG]", imapErr.Type, imapErr.Code)
	}

	// And no row was committed into the destination folder.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var dangling int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE f.mailbox_id = $1 AND f.name = 'Archive/2026'`,
		sess.mailboxID,
	).Scan(&dangling); err != nil {
		t.Fatalf("dangling count: %v", err)
	}
	if dangling != 0 {
		t.Errorf("COPY left %d dangling row(s) after the blob-missing failure", dangling)
	}
}
