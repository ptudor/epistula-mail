package imapsess

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
)

// TestConcurrentFolderDeleteKeepsQuotaExact is the RA6X-020 regression, driven
// by a barrier: two DELETEs of the same folder blocked on their
// accounting update, then released together.
//
// Folder identity and the byte total used to be read outside the transaction,
// so both deletes could compute the same 100-byte snapshot and both subtract
// it. The second affected zero folders and still committed its subtraction,
// leaving used_bytes below the real total — GREATEST(0, ...) hid the underflow
// rather than preventing it.
func TestConcurrentFolderDeleteKeepsQuotaExact(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 300 bytes in INBOX (the fixture) plus 100 copied into Victim.
	if _, err := sess.Copy(imap.UIDSetNum(1), "Victim"); err != nil {
		t.Fatalf("seed Victim: %v", err)
	}

	before := usedBytes(t, sess)
	if before != 400 {
		t.Fatalf("used_bytes = %d after seeding, want 400", before)
	}

	// Two independent sessions racing the same DELETE.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			peer := sess.be.NewSession()
			defer func() { _ = peer.Close() }()
			peer.mailboxID = sess.mailboxID
			peer.mailboxName = sess.mailboxName
			peer.tenant = sess.tenant
			<-start
			errs[i] = peer.Delete("Victim")
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one delete may report success; the other must report that the
	// folder is gone. Neither may double-count.
	var ok int
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 2 concurrent deletes succeeded; want exactly 1 (errs: %v)", ok, errs)
	}

	after := usedBytes(t, sess)
	if want := sumRawSize(t, ctx, sess); after != want {
		t.Fatalf("used_bytes = %d but SUM(messages.raw_size) = %d — the two deletes disagreed", after, want)
	}
	if after != 300 {
		t.Fatalf("used_bytes = %d, want 300 (only Victim's 100 bytes may be reclaimed)", after)
	}
}

// TestFailedFolderDeleteLeavesQuotaUnchanged pins the other half: a delete that
// does nothing must not move the counter.
func TestFailedFolderDeleteLeavesQuotaUnchanged(t *testing.T) {
	sess := mutationFixture(t)
	before := usedBytes(t, sess)

	if err := sess.Delete("NoSuchFolder"); err == nil {
		t.Fatal("deleting a nonexistent folder should fail")
	}
	if after := usedBytes(t, sess); after != before {
		t.Fatalf("used_bytes moved from %d to %d on a no-op delete", before, after)
	}
}

// TestStoreAndExpungeDoNotDeadlock is the RA6X-021 regression for the
// STORE/EXPUNGE pair. Under repeated concurrent load there must be no lock
// cycle: STORE bumps the folder then the messages, and EXPUNGE used to delete
// the messages then update the mailbox and the folder, so each could hold what
// the other wanted.
func TestStoreAndExpungeDoNotDeadlock(t *testing.T) {
	sess := mutationFixture(t)

	const rounds = 40
	var wg sync.WaitGroup
	storeErrs := make(chan error, rounds)
	expungeErrs := make(chan error, rounds)

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			peer := sess.be.NewSession()
			peer.mailboxID = sess.mailboxID
			peer.selectedFolderID = sess.selectedFolderID
			peer.selectedFolderName = sess.selectedFolderName
			err := peer.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
				Op:     imap.StoreFlagsAdd,
				Silent: true,
				Flags:  []imap.Flag{imap.FlagFlagged},
			}, nil)
			_ = peer.Close()
			if err != nil {
				storeErrs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			peer := sess.be.NewSession()
			peer.mailboxID = sess.mailboxID
			peer.selectedFolderID = sess.selectedFolderID
			peer.selectedFolderName = sess.selectedFolderName
			err := peer.Expunge(nil, nil)
			_ = peer.Close()
			if err != nil {
				expungeErrs <- err
			}
		}
	}()
	wg.Wait()
	close(storeErrs)
	close(expungeErrs)

	for err := range storeErrs {
		t.Errorf("STORE failed under concurrent EXPUNGE: %v", err)
	}
	for err := range expungeErrs {
		t.Errorf("EXPUNGE failed under concurrent STORE: %v", err)
	}
}

// TestOppositeDirectionMovesDoNotDeadlock pins the folder-ordering rule: two
// MOVEs in opposite directions between the same pair of folders are safe only
// because both sort the folder ids ascending before locking.
func TestOppositeDirectionMovesDoNotDeadlock(t *testing.T) {
	sess := mutationFixture(t)

	// Two folders, each holding one message.
	if _, err := sess.Copy(imap.UIDSetNum(1), "Left"); err != nil {
		t.Fatalf("seed Left: %v", err)
	}
	if _, err := sess.Copy(imap.UIDSetNum(2), "Right"); err != nil {
		t.Fatalf("seed Right: %v", err)
	}

	const rounds = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)

	move := func(from, to string) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			peer := sess.be.NewSession()
			peer.mailboxID = sess.mailboxID
			peer.mailboxName = sess.mailboxName
			peer.tenant = sess.tenant
			id, err := folderIDByName(sess, from)
			if err != nil {
				_ = peer.Close()
				continue
			}
			peer.selectedFolderID = id
			peer.selectedFolderName = from
			if err := peer.Move(nil, imap.UIDSetNum(1), to); err != nil {
				var ierr *imap.Error
				// "no matching messages" just means the other direction got
				// there first; only a real failure counts.
				if ok := asIMAPError(err, &ierr); !ok || ierr.Text != "MOVE: no matching messages" {
					errs <- err
				}
			}
			_ = peer.Close()
		}
	}

	wg.Add(2)
	go move("Left", "Right")
	go move("Right", "Left")
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("MOVE failed under an opposite-direction MOVE: %v", err)
	}
}

// TestRetryTxOnlyRetriesTransactionRollbacks pins the retry predicate: a
// deadlock is retried, a constraint violation is not (retrying it would turn
// one clear failure into three).
func TestRetryTxOnlyRetriesTransactionRollbacks(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A duplicate key: SQLSTATE 23505, not retryable.
	attempts := 0
	err := retryTx(ctx, sess.be.Pool.Begin, func(tx pgx.Tx) error {
		attempts++
		_, err := tx.Exec(ctx,
			`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x')`, sess.mailboxName)
		return err
	})
	if err == nil {
		t.Fatal("a duplicate mailbox name should have failed")
	}
	if attempts != 1 {
		t.Fatalf("a constraint violation was attempted %d times; want 1", attempts)
	}
	if isRetryableTxError(err) {
		t.Fatalf("a unique violation was classified as retryable: %v", err)
	}
}

// sumRawSize is the authoritative byte total the mailbox counter must equal.
func sumRawSize(t *testing.T, ctx context.Context, sess *Session) int64 {
	t.Helper()
	var n int64
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(m.raw_size), 0)
		  FROM messages m
		  JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1`, sess.mailboxID,
	).Scan(&n); err != nil {
		t.Fatalf("sum raw_size: %v", err)
	}
	return n
}

// folderIDByName resolves a folder within the session's mailbox.
func folderIDByName(sess *Session, name string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var id int64
	err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = $2`, sess.mailboxID, name,
	).Scan(&id)
	return id, err
}

// asIMAPError is errors.As specialised, kept local so the test reads clearly.
func asIMAPError(err error, target **imap.Error) bool {
	return errors.As(err, target)
}
