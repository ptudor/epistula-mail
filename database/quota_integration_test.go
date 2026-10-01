package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestIngestEnforcesQuota is the R-029 regression: a mailbox with a quota
// rejects the message that pushes used_bytes over it (ErrOverQuota, rolled
// back so used_bytes is unchanged), while messages that fit are accepted and
// IgnoreQuota (the import path) bypasses enforcement.
func TestIngestEnforcesQuota(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash, quota_bytes) VALUES ('quotabox', 'x', 80) RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	readUsed := func() int64 {
		var u int64
		if err := db.Pool().QueryRow(ctx, `SELECT used_bytes FROM mailboxes WHERE id=$1`, mailboxID).Scan(&u); err != nil {
			t.Fatalf("read used_bytes: %v", err)
		}
		return u
	}

	ingestOne := func(body string, ignoreQuota bool) error {
		raw := []byte("From: a@q.invalid\r\nSubject: " + body + "\r\n\r\n" + body + "\r\n")
		msg, err := gcTestParser().Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = db.Ingest(ctx, storage.IngestParams{
			MailboxID:    mailboxID,
			MailboxName:  "quotabox",
			FolderName:   "INBOX",
			EnvelopeFrom: "a@q.invalid",
			EnvelopeTo:   "quotabox@q.invalid",
			RawSHA256Hex: msg.SHA256Hex,
			RawSize:      int64(len(raw)),
			RawBlobDate:  time.Now().UTC(),
			Message:      msg,
			IgnoreQuota:  ignoreQuota,
		})
		return err
	}

	// First small message (~38 bytes) fits under quota 80.
	if err := ingestOne("aa", false); err != nil {
		t.Fatalf("first ingest (should fit): %v", err)
	}
	usedAfterFirst := readUsed()
	if usedAfterFirst == 0 || usedAfterFirst > 80 {
		t.Fatalf("used_bytes after first = %d, expected 0 < x <= 80", usedAfterFirst)
	}

	// A large second message pushes over 80 → ErrOverQuota, rolled back.
	err := ingestOne("this body is deliberately long enough to exceed the tiny quota", false)
	if !errors.Is(err, storage.ErrOverQuota) {
		t.Fatalf("over-quota ingest = %v, want ErrOverQuota", err)
	}
	if u := readUsed(); u != usedAfterFirst {
		t.Fatalf("used_bytes = %d after a rejected ingest, want unchanged %d", u, usedAfterFirst)
	}

	// IgnoreQuota (import/migration) bypasses enforcement even when over.
	if err := ingestOne("import restores existing mail regardless of quota size", true); err != nil {
		t.Fatalf("IgnoreQuota ingest should bypass quota: %v", err)
	}
	if u := readUsed(); u <= usedAfterFirst {
		t.Fatalf("used_bytes = %d after IgnoreQuota ingest, want it to have grown past %d", u, usedAfterFirst)
	}
}

// TestIngestNullQuotaUnlimited confirms a NULL quota_bytes means unlimited.
func TestIngestNullQuotaUnlimited(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('unlimited', 'x') RETURNING id`, // quota_bytes NULL
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	raw := []byte("From: a@q.invalid\r\nSubject: big\r\n\r\n" + string(make([]byte, 5000)) + "\r\n")
	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID: mailboxID, MailboxName: "unlimited", FolderName: "INBOX",
		EnvelopeFrom: "a@q.invalid", EnvelopeTo: "unlimited@q.invalid",
		RawSHA256Hex: msg.SHA256Hex, RawSize: int64(len(raw)), RawBlobDate: time.Now().UTC(),
		Message: msg,
	}); err != nil {
		t.Fatalf("ingest into NULL-quota mailbox should always succeed: %v", err)
	}
}

// TestOverQuotaPreCheckDeliverSide is the deliver half of RO5X-009.
//
// The LDA writes the raw blob (and every attachment blob) to disk before it
// opens the ingest tx, and the quota check lives inside that tx. A rollback
// leaves the blobs behind; `gc mark` then skips them for the 24 h grace, so a
// sender who knows a mailbox is over quota could park
// min(message_size_limit, max_message_bytes) bytes per attempt on the spool
// for over a day, paying only a bounced SMTP transaction each time.
//
// The advisory pre-check stops the write before it happens. It is
// deliberately fail-open, so this also pins that it never rejects on its own.
func TestOverQuotaPreCheckDeliverSide(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash, quota_bytes)
		 VALUES ('precheck', 'x', 1000) RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	// Nothing used yet: everything up to the quota passes.
	for _, tc := range []struct {
		name string
		size int64
		want bool
	}{
		{"well under", 100, false},
		{"exactly at quota", 1000, false}, // used+size == quota is allowed
		{"one over", 1001, true},
		{"far over", 50 << 20, true},
	} {
		if got := overQuotaPreCheck(ctx, db, mailboxID, tc.size); got != tc.want {
			t.Errorf("%s: overQuotaPreCheck(%d) = %v, want %v", tc.name, tc.size, got, tc.want)
		}
	}

	// Consume most of the quota, then re-check.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET used_bytes = 900 WHERE id = $1`, mailboxID); err != nil {
		t.Fatalf("set used_bytes: %v", err)
	}
	if overQuotaPreCheck(ctx, db, mailboxID, 100) {
		t.Error("pre-check refused a message that exactly fills the remaining quota")
	}
	if !overQuotaPreCheck(ctx, db, mailboxID, 101) {
		t.Error("pre-check allowed a message that exceeds the remaining quota")
	}

	// NULL quota means unlimited.
	var unlimitedID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('unlimited', 'x') RETURNING id`,
	).Scan(&unlimitedID); err != nil {
		t.Fatalf("insert unlimited mailbox: %v", err)
	}
	if overQuotaPreCheck(ctx, db, unlimitedID, 50<<20) {
		t.Error("pre-check refused against a NULL (unlimited) quota")
	}

	// Fail-open: an unknown mailbox id yields no rows, and the helper must
	// defer to the authoritative in-tx check rather than rejecting mail.
	if overQuotaPreCheck(ctx, db, -1, 50<<20) {
		t.Error("pre-check must fail open on a query error; it must never reject mail on its own")
	}
}

// TestOverQuotaPreCheckMatchesInTxComparison guards the two checks against
// drifting apart: a pre-check stricter than the tx would bounce mail the tx
// would have accepted.
func TestOverQuotaPreCheckMatchesInTxComparison(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash, quota_bytes)
		 VALUES ('drift', 'x', 200) RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	ingestOne := func(raw []byte) error {
		msg, err := gcTestParser().Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = db.Ingest(ctx, storage.IngestParams{
			MailboxID:    mailboxID,
			MailboxName:  "drift",
			FolderName:   "INBOX",
			EnvelopeFrom: "a@d.invalid",
			EnvelopeTo:   "drift@d.invalid",
			RawSHA256Hex: msg.SHA256Hex,
			RawSize:      int64(len(raw)),
			RawBlobDate:  time.Now().UTC(),
			Message:      msg,
		})
		return err
	}

	// Walk sizes across the boundary; the pre-check's verdict must agree
	// with what Ingest actually does.
	for _, size := range []int{60, 80, 100} {
		body := make([]byte, size)
		for i := range body {
			body[i] = 'a'
		}
		raw := append([]byte("From: a@d.invalid\r\nSubject: s\r\n\r\n"), body...)

		pre := overQuotaPreCheck(ctx, db, mailboxID, int64(len(raw)))
		err := ingestOne(raw)
		txRejected := errors.Is(err, storage.ErrOverQuota)
		if err != nil && !txRejected {
			t.Fatalf("ingest size %d: %v", size, err)
		}
		if pre != txRejected {
			t.Errorf("size %d (raw %d bytes): pre-check said over=%v but the tx said over=%v — "+
				"the two comparisons have drifted apart", size, len(raw), pre, txRejected)
		}
	}
}
