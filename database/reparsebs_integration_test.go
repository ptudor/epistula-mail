package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestReparseBodystructureBackfillsStaleSizes is the RO5X-003 back-fill
// verification: a row carrying the pre-fix (decoded-length) bodystructure is
// rewritten with the corrected encoded-length value, derived from the
// immutable raw blob.
func TestReparseBodystructureBackfillsStaleSizes(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := t.TempDir()
	store := blob.NewStore(root)
	parser := ingest.New(ingest.DefaultLimits())

	// A message with a base64 part, so encoded and decoded lengths differ.
	payload := []byte(strings.Repeat("backfill payload ", 60))
	encoded := base64.StdEncoding.EncodeToString(payload)
	raw := []byte("From: s@bf.invalid\r\nTo: r@bf.invalid\r\nSubject: bf\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		encoded + "\r\n--B--\r\n")

	msg, err := parser.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Seed a mailbox, folder, blob, and a messages row whose bodystructure
	// is the STALE shape: part sizes overwritten with the decoded length.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', '$argon2id$ph')`); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM mailboxes WHERE name='alice'`).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox id: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1,'INBOX',1,2) RETURNING id`, mailboxID).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}

	when := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	tenant, err := blob.ParseTenant("alice")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	w, err := store.NewWriter(blob.KindRaw, tenant, blob.BucketFromTime(when))
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if _, _, _, err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}

	// Build the stale bodystructure: same tree, decoded sizes on leaves.
	stale := staleify(msg.BodyStructure, parser, raw)
	staleJSON, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale: %v", err)
	}

	shaBytes := shaOf(t, msg.SHA256Hex)
	var msgID int64
	if err := db.Pool().QueryRow(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
		) VALUES ($1,1,$2,$3,$4,$5,'bf','s@bf.invalid','{}','{}','{}','hello',$6,'{}')
		RETURNING id`,
		folderID, shaBytes, when, int64(len(raw)), when, staleJSON,
	).Scan(&msgID); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Dry run must report the change without writing it.
	st, err := reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10, DryRun: true})
	if err != nil {
		t.Fatalf("reparse dry-run: %v", err)
	}
	if st.Rewritten != 1 || st.Scanned != 1 {
		t.Errorf("dry-run stats = %+v, want 1 scanned / 1 rewritten", st)
	}
	// Compare values, not bytes: Postgres normalizes JSONB key order and
	// whitespace on storage, so the round-tripped bytes never match the
	// input verbatim even when the value is untouched.
	if got := readBS(t, db, msgID); !jsonEquivalent(got, staleJSON) {
		t.Errorf("dry-run modified the row: got %s", got)
	}

	// Real run rewrites it.
	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if st.Rewritten != 1 {
		t.Errorf("stats = %+v, want 1 rewritten", st)
	}

	var got ingest.BodyStructure
	if err := json.Unmarshal(readBS(t, db, msgID), &got); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("rewritten Parts = %d, want 2", len(got.Parts))
	}
	if got.Parts[1].Size != msg.BodyStructure.Parts[1].Size {
		t.Errorf("rewritten size = %d, want the freshly-parsed %d",
			got.Parts[1].Size, msg.BodyStructure.Parts[1].Size)
	}
	if got.Parts[1].Size <= int64(len(payload)) {
		t.Errorf("rewritten size = %d, want > decoded length %d (encoded length expected)",
			got.Parts[1].Size, len(payload))
	}

	// Idempotent: a second pass rewrites nothing.
	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("reparse (second pass): %v", err)
	}
	if st.Rewritten != 0 || st.Unchanged != 1 {
		t.Errorf("second pass stats = %+v, want 0 rewritten / 1 unchanged", st)
	}
}

// TestReparseToleratesMissingBlob proves one unreadable blob does not abort a
// back-fill pass over a large archive.
func TestReparseToleratesMissingBlob(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	parser := ingest.New(ingest.DefaultLimits())

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', '$argon2id$ph')`); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM mailboxes WHERE name='alice'`).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox id: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1,'INBOX',1,2) RETURNING id`, mailboxID).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}
	sha := make([]byte, 32)
	for i := range sha {
		sha[i] = 0xab
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
		) VALUES ($1,1,$2,$3,10,$4,'x','s@x.invalid','{}','{}','{}','x','{}','{}')`,
		folderID, sha, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	st, err := reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("reparse returned an error for a missing blob; it should skip: %v", err)
	}
	if st.BlobMissing != 1 || st.Rewritten != 0 {
		t.Errorf("stats = %+v, want 1 blob_missing / 0 rewritten", st)
	}
}

func TestJSONEquivalent(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", `{"a":1,"b":[2,3]}`, `{"a":1,"b":[2,3]}`, true},
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"whitespace", `{"a": 1}`, `{"a":1}`, true},
		{"different value", `{"a":1}`, `{"a":2}`, false},
		{"array order matters", `{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{"missing key", `{"a":1}`, `{"a":1,"b":2}`, false},
		{"empty current", ``, `{"a":1}`, false},
	} {
		if got := jsonEquivalent([]byte(tc.a), []byte(tc.b)); got != tc.want {
			t.Errorf("%s: jsonEquivalent(%q,%q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// staleify rebuilds the pre-fix bodystructure: leaf Size overwritten with the
// decoded length, reproducing what the old walker persisted.
func staleify(bs ingest.BodyStructure, parser *ingest.Parser, raw []byte) ingest.BodyStructure {
	out := bs
	if len(bs.Parts) > 0 {
		out.Parts = make([]ingest.BodyStructure, len(bs.Parts))
		for i, p := range bs.Parts {
			out.Parts[i] = staleify(p, parser, raw)
		}
		return out
	}
	// Halve the leaf size — any value differing from the correct one proves
	// the back-fill actually rewrote it.
	out.Size = bs.Size / 2
	return out
}

func readBS(t *testing.T, db *storage.DB, id int64) []byte {
	t.Helper()
	var b []byte
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT bodystructure FROM messages WHERE id = $1`, id).Scan(&b); err != nil {
		t.Fatalf("read bodystructure: %v", err)
	}
	return b
}

func shaOf(t *testing.T, hexStr string) []byte {
	t.Helper()
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("sha hex: %v", err)
	}
	return b
}
