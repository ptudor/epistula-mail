package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestOversizedTextMessageIngestsAndIsRetrievable is the RA6X-030 regression.
//
// messages.fts is a STORED generated column, so its expression runs inside the
// INSERT. PostgreSQL caps a tsvector at 1 MiB while the parser accepts 50 MiB
// messages, so a message far under the size limit could be rejected outright
// because its search vector did not fit — and once RA6X-008 made SQL failures
// defer instead of bounce, it would have retried forever instead.
//
// The body here has the shape that triggers it: high-cardinality unique
// tokens, ~1.7 MB, which produced a 1.81 MB vector and
// "string is too long for tsvector".
func TestOversizedTextMessageIngestsAndIsRetrievable(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('ftsbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	// ~1.7 MB of unique 32-character tokens.
	var body strings.Builder
	for i := 0; i < 50000; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprint(i)))
		body.WriteString(fmt.Sprintf("%x ", sum[:16]))
	}
	// A marker inside the indexed prefix and one far beyond it, so the
	// documented coverage boundary is asserted rather than assumed.
	raw := "From: s@x.invalid\r\nSubject: EARLYMARKER huge\r\n\r\n" +
		"EARLYMARKER-IN-PREFIX " + body.String() + " LATEMARKER-BEYOND-PREFIX\r\n"
	if len(raw) < 1_500_000 {
		t.Fatalf("fixture is only %d bytes; it must exceed the tsvector limit", len(raw))
	}

	parser := gcTestParser()
	msg, err := parser.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	when := time.Now().UTC()
	w, err := store.NewWriter(blob.KindRaw, "ftsbox", blob.BucketFromTime(when))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mailboxID,
		MailboxName:  "ftsbox",
		FolderName:   "INBOX",
		EnvelopeTo:   "ftsbox@x.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  when,
		Message:      msg,
	})
	if err != nil {
		t.Fatalf("Ingest of a %d-byte text message failed: %v", len(raw), err)
	}

	// Stored in full: text_body keeps the whole projection, and the quota
	// counts the real size.
	var storedLen int64
	var storedSize int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT length(text_body), raw_size FROM messages WHERE id = $1`, res.MessageID,
	).Scan(&storedLen, &storedSize); err != nil {
		t.Fatalf("read stored message: %v", err)
	}
	if storedLen < 1_000_000 {
		t.Fatalf("text_body was truncated to %d characters; only the INDEX may be bounded", storedLen)
	}
	if storedSize != int64(len(raw)) {
		t.Fatalf("raw_size = %d, want %d", storedSize, len(raw))
	}
	var used int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, mailboxID).Scan(&used); err != nil {
		t.Fatalf("read quota: %v", err)
	}
	if used != int64(len(raw)) {
		t.Fatalf("used_bytes = %d, want %d", used, len(raw))
	}

	// Documented coverage: a term inside the indexed prefix matches; one
	// beyond it does not. Both are still STORED and retrievable.
	for _, tc := range []struct {
		term      string
		wantMatch bool
	}{
		{"earlymarker", true},
		{"latemarker", false},
	} {
		var n int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM messages
			  WHERE id = $1 AND fts @@ plainto_tsquery('simple', $2)`,
			res.MessageID, tc.term,
		).Scan(&n); err != nil {
			t.Fatalf("search %q: %v", tc.term, err)
		}
		if (n > 0) != tc.wantMatch {
			t.Errorf("search %q matched=%v, want %v (the indexed prefix is 200,000 characters)",
				tc.term, n > 0, tc.wantMatch)
		}
	}

	// The out-of-index term is nonetheless present in the stored text.
	var present bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT position('LATEMARKER-BEYOND-PREFIX' in text_body) > 0 FROM messages WHERE id = $1`,
		res.MessageID,
	).Scan(&present); err != nil {
		t.Fatalf("locate late marker: %v", err)
	}
	if !present {
		t.Error("text beyond the indexed prefix was not stored")
	}
}

// TestOrdinaryMessageSearchIsUnchanged pins that normal mail — every message
// anyone actually sends — indexes and matches exactly as before.
func TestOrdinaryMessageSearchIsUnchanged(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('ordinary', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 2) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO messages (folder_id, uid, raw_sha256, raw_size, internal_date, subject, text_body)
		VALUES ($1, 1, decode(repeat('ab',32),'hex'), 10, now(), 'quarterly invoice', 'please find the invoice attached')`,
		folderID,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	for _, term := range []string{"quarterly", "invoice", "attached"} {
		var n int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM messages WHERE folder_id = $1 AND fts @@ plainto_tsquery('simple', $2)`,
			folderID, term,
		).Scan(&n); err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		if n != 1 {
			t.Errorf("ordinary search for %q matched %d messages, want 1", term, n)
		}
	}
}
