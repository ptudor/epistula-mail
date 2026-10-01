package imapsess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// setQuota gives the session's mailbox a byte quota.
func setQuota(t *testing.T, sess *Session, quota int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE mailboxes SET quota_bytes = $1 WHERE id = $2`, quota, sess.mailboxID,
	); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

func usedBytesOf(t *testing.T, sess *Session) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var used int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, sess.mailboxID,
	).Scan(&used); err != nil {
		t.Fatalf("read used_bytes: %v", err)
	}
	return used
}

// TestAppendOverQuotaWritesNoBlob is the RO5X-009 regression.
//
// Both write paths committed the raw blob to disk before opening the ingest
// tx, and the quota check lives inside that tx. A rejected message therefore
// left a full-size blob that nothing unlinked: `gc mark` skips files younger
// than the 24 h grace, so the orphan survived at least a day. A client
// looping APPEND into an over-quota mailbox could park arbitrary bytes on the
// spool — the opposite of what a quota is for.
func TestAppendOverQuotaWritesNoBlob(t *testing.T) {
	sess, store := appendFixture(t)
	mustCreateFolder(t, sess, "INBOX")

	msgSize := int64(len(appendRawMsg))
	// Room for exactly one message.
	setQuota(t, sess, msgSize)

	if _, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatalf("first APPEND (within quota): %v", err)
	}
	blobsAfterFirst := countBlobFiles(t, store.Root())
	usedAfterFirst := usedBytesOf(t, sess)

	// The second message exceeds the quota.
	second := appendRawMsg + "\r\nsecond message body\r\n"
	_, err := sess.Append("INBOX", newLiteral([]byte(second)), nil)
	if err == nil {
		t.Fatal("second APPEND succeeded despite exceeding the quota")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("err = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeOverQuota {
		t.Errorf("Code = %q, want OVERQUOTA", imapErr.Code)
	}

	// The rejection must not have left a blob behind.
	if after := countBlobFiles(t, store.Root()); after != blobsAfterFirst {
		t.Errorf("blob count went %d -> %d; an over-quota APPEND must write no blob (RO5X-009)",
			blobsAfterFirst, after)
	}
	// And must not have moved the accounting.
	if got := usedBytesOf(t, sess); got != usedAfterFirst {
		t.Errorf("used_bytes = %d, want %d (the rejected message must not count)", got, usedAfterFirst)
	}
}

// TestAppendUnlimitedQuotaUnaffected keeps the NULL-quota (unlimited) path
// working — the pre-check must skip it entirely.
func TestAppendUnlimitedQuotaUnaffected(t *testing.T) {
	sess, _ := appendFixture(t)
	mustCreateFolder(t, sess, "INBOX")

	// quota_bytes defaults to NULL; append several messages.
	for i := 0; i < 3; i++ {
		if _, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), nil); err != nil {
			t.Fatalf("APPEND %d with unlimited quota: %v", i, err)
		}
	}
}

// TestAppendPreCheckIsAdvisoryNotAuthoritative pins the fail-open contract:
// the pre-check must never be the sole reason an APPEND is refused. Right at
// the boundary (exactly filling the quota) the message must still be accepted,
// proving the pre-check uses the same comparison as the in-tx check rather
// than a stricter one.
func TestAppendPreCheckIsAdvisoryNotAuthoritative(t *testing.T) {
	sess, _ := appendFixture(t)
	mustCreateFolder(t, sess, "INBOX")

	msgSize := int64(len(appendRawMsg))
	setQuota(t, sess, msgSize) // exactly enough for one

	if _, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatalf("APPEND that exactly fills the quota was refused: %v", err)
	}
	if got := usedBytesOf(t, sess); got != msgSize {
		t.Errorf("used_bytes = %d, want %d", got, msgSize)
	}
}

// TestOverQuotaPreCheckBoundaries exercises the helper directly.
func TestOverQuotaPreCheckBoundaries(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// NULL quota = unlimited.
	if sess.overQuotaPreCheck(ctx, 1<<30) {
		t.Error("pre-check rejected against a NULL (unlimited) quota")
	}

	setQuota(t, sess, 1000)
	for _, tc := range []struct {
		name string
		size int64
		want bool
	}{
		{"well under", 100, false},
		{"exactly at quota", 1000, false}, // used+size == quota is allowed
		{"one over", 1001, true},
		{"far over", 1 << 20, true},
	} {
		if got := sess.overQuotaPreCheck(ctx, tc.size); got != tc.want {
			t.Errorf("%s: overQuotaPreCheck(%d) = %v, want %v", tc.name, tc.size, got, tc.want)
		}
	}

	// A mailbox id that does not exist makes the query return no rows; the
	// helper must fail open rather than refusing.
	saved := sess.mailboxID
	sess.mailboxID = -1
	if sess.overQuotaPreCheck(ctx, 1<<30) {
		t.Error("pre-check must fail open on a query error, not refuse")
	}
	sess.mailboxID = saved
}
