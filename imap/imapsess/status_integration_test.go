package imapsess

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// newStatusFixture creates an INBOX with a mix of seen/unseen/deleted messages
// so STATUS aggregates are non-trivial. No blob store is needed — STATUS never
// touches disk. Returns a Session ready for Status/Select.
func newStatusFixture(t *testing.T) (*Session, *pgxpool.Pool, int64) {
	t.Helper()
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', '$argon2id$placeholder') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	var folderID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 6) RETURNING id`,
		mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	// UID 1: \Seen; UID 2: \Seen; UID 3: unseen; UID 4: unseen \Deleted;
	// UID 5: \Seen \Deleted. => 5 messages, 2 unseen, 2 deleted.
	rows := []struct {
		uid   int64
		flags []string
		size  int64
	}{
		{1, []string{`\Seen`}, 100},
		{2, []string{`\Seen`}, 200},
		{3, []string{}, 300},
		{4, []string{`\Deleted`}, 400},
		{5, []string{`\Seen`, `\Deleted`}, 500},
	}
	for _, r := range rows {
		sha := make([]byte, 32)
		sha[0] = byte(r.uid)
		if _, err := pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				headers, bodystructure, flags
			) VALUES ($1, $2, $3, $4, $5, $6, '{}', '{}', $7)`,
			folderID, r.uid, sha, time.Now(), r.size, time.Now(), r.flags,
		); err != nil {
			t.Fatalf("insert message uid=%d: %v", r.uid, err)
		}
	}

	be := &Backend{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout: 10 * time.Second,
	}
	sess := be.NewSession()
	sess.mailboxID = mailboxID
	sess.mailboxName = "alice"
	sess.tenant = "alice"
	return sess, pool, folderID
}

// TestStatusPopulatesRequestedPointers is the R-002 regression guard: STATUS
// must set every requested pointer field non-nil (the go-imap writer
// dereferences them unconditionally) and report the correct UNSEEN count.
func TestStatusPopulatesRequestedPointers(t *testing.T) {
	sess, _, _ := newStatusFixture(t)

	data, err := sess.Status("INBOX", &imap.StatusOptions{
		NumMessages: true,
		NumUnseen:   true,
		NumRecent:   true,
		NumDeleted:  true,
		Size:        true,
		UIDNext:     true,
		UIDValidity: true,
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if data.NumMessages == nil || *data.NumMessages != 5 {
		t.Errorf("NumMessages = %v, want 5", data.NumMessages)
	}
	if data.NumUnseen == nil || *data.NumUnseen != 2 {
		t.Errorf("NumUnseen = %v, want 2", data.NumUnseen)
	}
	if data.NumDeleted == nil || *data.NumDeleted != 2 {
		t.Errorf("NumDeleted = %v, want 2", data.NumDeleted)
	}
	if data.Size == nil || *data.Size != 1500 {
		t.Errorf("Size = %v, want 1500", data.Size)
	}
	// RECENT is not tracked but must be a non-nil zero so the writer can emit
	// it without panicking.
	if data.NumRecent == nil {
		t.Errorf("NumRecent is nil; must be non-nil (0) when requested")
	}
}

// TestStatusUnseenAndDeletedNilWhenNotRequested confirms we don't attach
// pointers the client didn't ask for (the writer only emits requested items,
// but this keeps the response minimal and matches the option contract).
func TestStatusUnseenNilWhenNotRequested(t *testing.T) {
	sess, _, _ := newStatusFixture(t)
	data, err := sess.Status("INBOX", &imap.StatusOptions{NumMessages: true, UIDNext: true})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if data.NumUnseen != nil {
		t.Errorf("NumUnseen = %v, want nil (not requested)", *data.NumUnseen)
	}
	if data.NumRecent != nil {
		t.Errorf("NumRecent = %v, want nil (not requested)", *data.NumRecent)
	}
}

// TestSelectReportsFirstUnseen confirms Select computes the sequence number of
// the first unseen message (UID 3 => seq 3 in this fixture).
func TestSelectReportsFirstUnseen(t *testing.T) {
	sess, _, _ := newStatusFixture(t)
	data, err := sess.Select("INBOX", &imap.SelectOptions{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if data.NumMessages != 5 {
		t.Errorf("NumMessages = %d, want 5", data.NumMessages)
	}
	if data.FirstUnseenSeqNum != 3 {
		t.Errorf("FirstUnseenSeqNum = %d, want 3", data.FirstUnseenSeqNum)
	}
}
