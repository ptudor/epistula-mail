package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestUIDValiditySequenceIsNotConsumedByExistingFolders is the first half of
// RA6X-052.
//
// `INSERT ... VALUES (..., nextval('folder_uidvalidity_seq')) ON CONFLICT DO
// NOTHING` still EVALUATES nextval when the row already exists, so the
// UIDVALIDITY sequence was consumed once per DELIVERY rather than once per
// folder creation — tying its consumption to message traffic, in a value that
// must fit a 32-bit protocol field and is seeded near the current Unix epoch.
func TestUIDValiditySequenceIsNotConsumedByExistingFolders(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('seqbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	deliver := func(n int) {
		t.Helper()
		raw := []byte("From: s@x.invalid\r\nSubject: m" + string(rune('a'+n)) + "\r\n\r\nbody\r\n")
		msg, err := gcTestParser().Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		when := time.Now().UTC()
		w, err := store.NewWriter(blob.KindRaw, "seqbox", blob.BucketFromTime(when))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatalf("Write: %v", err)
		}
		sha, _, _, err := w.Close()
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := db.Ingest(ctx, storage.IngestParams{
			MailboxID: mailboxID, MailboxName: "seqbox", FolderName: "INBOX",
			EnvelopeTo: "seqbox@x.invalid", RawSHA256Hex: sha,
			RawSize: int64(len(raw)), RawBlobDate: when, Message: msg,
		}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	// First delivery creates INBOX and legitimately consumes one value.
	deliver(0)
	var afterFirst int64
	if err := db.Pool().QueryRow(ctx, `SELECT last_value FROM folder_uidvalidity_seq`).Scan(&afterFirst); err != nil {
		t.Fatalf("read sequence: %v", err)
	}

	// Ten more deliveries into the SAME folder must not consume any.
	for i := 1; i <= 10; i++ {
		deliver(i)
	}
	var afterMore int64
	if err := db.Pool().QueryRow(ctx, `SELECT last_value FROM folder_uidvalidity_seq`).Scan(&afterMore); err != nil {
		t.Fatalf("read sequence: %v", err)
	}
	if afterMore != afterFirst {
		t.Fatalf("the UIDVALIDITY sequence advanced by %d over 10 deliveries into an existing folder; "+
			"it must only advance when a folder is created", afterMore-afterFirst)
	}

	// Creating a second folder does consume one.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Archive', nextval('folder_uidvalidity_seq'), 1)`, mailboxID); err != nil {
		t.Fatalf("create second folder: %v", err)
	}
	var afterSecond int64
	if err := db.Pool().QueryRow(ctx, `SELECT last_value FROM folder_uidvalidity_seq`).Scan(&afterSecond); err != nil {
		t.Fatalf("read sequence: %v", err)
	}
	if afterSecond <= afterMore {
		t.Fatal("creating a folder did not consume a UIDVALIDITY")
	}
}

// TestProtocolIDsAreRefusedBeforeTheyWrap is the second half of RA6X-052.
//
// UID and UIDVALIDITY are BIGINT columns fed by an unrestricted sequence, but
// IMAP carries them in 32-bit fields. An unchecked conversion emits zero —
// which RFC 9051 forbids for UIDVALIDITY — or reuses an identifier a client has
// cached, which is silently worse: the client believes its cache is valid and
// shows the wrong messages. The allocation is refused before commit, so the
// failed delivery leaves the message, the quota and the counters untouched.
func TestProtocolIDsAreRefusedBeforeTheyWrap(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  int64
		ok   bool
	}{
		{"one", 1, true},
		{"the largest representable value", math.MaxUint32, true},
		{"one past the range", math.MaxUint32 + 1, false},
		{"zero is reserved", 0, false},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.CheckProtocolIDForTest("uid", tc.val)
			if tc.ok && err != nil {
				t.Fatalf("%d was refused: %v", tc.val, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%d was accepted", tc.val)
			}
		})
	}
}

// TestExhaustedUIDDoesNotCommit pins the end-to-end behaviour: a folder whose
// uidnext has reached the protocol limit refuses the delivery and leaves the
// mailbox exactly as it was.
func TestExhaustedUIDDoesNotCommit(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('wrapbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, $2) RETURNING id`, mailboxID, int64(math.MaxUint32)+1,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	raw := []byte("From: s@x.invalid\r\nSubject: wrap\r\n\r\nbody\r\n")
	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	when := time.Now().UTC()
	w, _ := store.NewWriter(blob.KindRaw, "wrapbox", blob.BucketFromTime(when))
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, _ := w.Close()

	_, err = db.Ingest(ctx, storage.IngestParams{
		MailboxID: mailboxID, MailboxName: "wrapbox", FolderName: "INBOX",
		EnvelopeTo: "wrapbox@x.invalid", RawSHA256Hex: sha,
		RawSize: int64(len(raw)), RawBlobDate: when, Message: msg,
	})
	if err == nil {
		t.Fatal("a delivery allocating a UID beyond the 32-bit range succeeded")
	}
	if !strings.Contains(err.Error(), "32-bit") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Nothing committed: no message, and the quota is untouched.
	var messages, used int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT (SELECT count(*) FROM messages WHERE folder_id = $1),
		        (SELECT used_bytes FROM mailboxes WHERE id = $2)`,
		folderID, mailboxID,
	).Scan(&messages, &used); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if messages != 0 || used != 0 {
		t.Fatalf("a refused allocation left %d message(s) and used_bytes=%d", messages, used)
	}
}
