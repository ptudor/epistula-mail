package imapsess

import (
	"errors"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestCopyEnforcesQuota is the RO5X-011 regression.
//
// copyInTx incremented the destination mailbox's used_bytes per copied
// message and never compared it to quota_bytes, so an authenticated user at
// 100% of quota could `COPY 1:* Archive` repeatedly and grow the counter
// without bound. No new blobs are written (COPY is intra-mailbox), but
// used_bytes is what epistula-api reports and `gc reconcile-quotas` reconciles,
// and once inflated, delivery starts bouncing EX_CANTCREAT against a quota
// the user manufactured.
func TestCopyEnforcesQuota(t *testing.T) {
	sess := mutationFixture(t)

	// The fixture inserts 3 messages of 100 bytes each; used_bytes is 300.
	before := usedBytes(t, sess)
	setQuota(t, sess, before) // exactly full

	_, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/2026")
	if err == nil {
		t.Fatal("COPY succeeded against a full quota")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("err = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeOverQuota {
		t.Errorf("Code = %q, want OVERQUOTA", imapErr.Code)
	}

	// All-or-nothing: the whole tx rolled back.
	if got := usedBytes(t, sess); got != before {
		t.Errorf("used_bytes = %d, want %d (the COPY must roll back entirely)", got, before)
	}
	if folderCount(t, sess, "Archive/2026") != 0 {
		t.Error("the destination folder survived a rolled-back COPY")
	}
}

// TestCopyWithinQuotaSucceeds keeps the normal path working and confirms the
// accounting still moves by the exact sum.
func TestCopyWithinQuotaSucceeds(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)
	setQuota(t, sess, before+200) // room for exactly two 100-byte messages

	data, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/2026")
	if err != nil {
		t.Fatalf("COPY within quota: %v", err)
	}
	if data == nil {
		t.Fatal("CopyData is nil")
	}
	if got, want := usedBytes(t, sess), before+200; got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}
}

// TestCopyUnlimitedQuotaUnaffected — NULL quota means unlimited.
func TestCopyUnlimitedQuotaUnaffected(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)

	if _, err := sess.Copy(imap.UIDSetNum(1, 2, 3), "Archive/2026"); err != nil {
		t.Fatalf("COPY with unlimited quota: %v", err)
	}
	if got, want := usedBytes(t, sess), before+300; got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}
}

// TestMoveIsQuotaNeutral is the other half of the fix: a MOVE within the same
// mailbox adds and frees the same bytes, so it must never trip the quota —
// not even transiently, and not even when the mailbox is exactly full.
func TestMoveIsQuotaNeutral(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)
	setQuota(t, sess, before) // exactly full: a transient double-count would fail

	if err := sess.Move(nil, imap.UIDSetNum(1, 2), "Archive/2026"); err != nil {
		t.Fatalf("MOVE at exactly-full quota was refused; it is quota-neutral: %v", err)
	}
	if got := usedBytes(t, sess); got != before {
		t.Errorf("used_bytes = %d, want %d (MOVE is quota-neutral in aggregate)", got, before)
	}
}

// TestMoveAllowedWhenAlreadyOverQuota covers the operator-lowered-quota case:
// a mailbox already above its quota must still be reorganizable, because MOVE
// does not grow it.
func TestMoveAllowedWhenAlreadyOverQuota(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)
	setQuota(t, sess, before/2) // operator lowered the quota below current usage

	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Errorf("MOVE refused on an already-over-quota mailbox; it frees as much as it adds: %v", err)
	}
	if got := usedBytes(t, sess); got != before {
		t.Errorf("used_bytes = %d, want %d", got, before)
	}
}

// TestCopyQuotaCheckedOnceNotPerRow pins the "check once, after the loop"
// requirement: a copy that fits in aggregate must not be refused just
// because an intermediate row would have crossed the line.
func TestCopyQuotaCheckedOnceNotPerRow(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)
	setQuota(t, sess, before+300) // room for all three, exactly

	if _, err := sess.Copy(imap.UIDSetNum(1, 2, 3), "Archive/2026"); err != nil {
		t.Fatalf("COPY that exactly fills the quota was refused: %v", err)
	}
	if got, want := usedBytes(t, sess), before+300; got != want {
		t.Errorf("used_bytes = %d, want %d", got, want)
	}
}
