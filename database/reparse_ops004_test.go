package main

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestReparseRenumbersEnclosedAttachments is the OPS-004 back-fill. Ingest
// numbered the parts of a message whose own Content-Type is message/rfc822 one
// level short: an attachment the RFC 9051 numbering places at 1.2 was stored
// as part 2. Only attachments.part_number differs, so reparse-bodystructure,
// which compared the number of attachment rows, saw nothing to repair. It now
// compares the part numbers and replaces the rows; the structure, the blobs
// and every other message are left alone.
func TestReparseRenumbersEnclosedAttachments(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gcMustMailbox(t, ctx, db, "renumber")

	attachment := "Content-Type: multipart/mixed; boundary=att\r\n\r\n" +
		"--att\r\nContent-Type: text/plain\r\n\r\ntext\r\n" +
		"--att\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("attachment bytes")) + "\r\n--att--\r\n"
	forwarded := "From: outer@ops004.invalid\r\nSubject: forwarded\r\nContent-Type: message/rfc822\r\n\r\n" +
		"From: inner@ops004.invalid\r\nSubject: inner\r\n" + attachment
	control := "From: plain@ops004.invalid\r\nSubject: control\r\n" + attachment

	root, storageRoot := t.TempDir(), t.TempDir()
	writeMaildirMessage(t, root, "0001", forwarded)
	writeMaildirMessage(t, root, "0002", control)
	cfg := writeRenameConfig(t, dsn, storageRoot)
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "renumber"}); code != EX_OK {
		t.Fatalf("import exit %d", code)
	}

	parts := func() map[string]string {
		rows, err := db.Pool().Query(ctx, `
			SELECT m.subject, a.part_number FROM attachments a JOIN messages m ON m.id = a.message_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var subject, part string
			if err := rows.Scan(&subject, &part); err != nil {
				t.Fatal(err)
			}
			out[subject] = part
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := parts(); got["forwarded"] != "1.2" || got["control"] != "2" || len(got) != 2 {
		t.Fatalf("imported attachment parts = %v, want forwarded 1.2 and control 2", got)
	}

	// The row a pre-OPS-004 import wrote for the forwarded message.
	if _, err := db.Pool().Exec(ctx, `
		UPDATE attachments a SET part_number = '2' FROM messages m
		 WHERE m.id = a.message_id AND m.subject = 'forwarded'`); err != nil {
		t.Fatal(err)
	}
	var structureBefore []byte
	if err := db.Pool().QueryRow(ctx, `SELECT bodystructure FROM messages WHERE subject = 'forwarded'`).Scan(&structureBefore); err != nil {
		t.Fatal(err)
	}

	store := blob.NewStore(storageRoot)
	parser := ingest.NewSalvaging(ingest.DefaultLimits())
	st, err := reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if st.Rewritten != 1 || st.Unchanged != 1 {
		t.Errorf("dry run: %+v, want 1 to rewrite and 1 unchanged", st)
	}
	if got := parts(); got["forwarded"] != "2" {
		t.Fatalf("the dry run changed the stored part number: %v", got)
	}

	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if st.Rewritten != 1 || st.Unchanged != 1 {
		t.Errorf("reparse: %+v, want 1 rewritten and 1 unchanged", st)
	}
	if got := parts(); got["forwarded"] != "1.2" || got["control"] != "2" || len(got) != 2 {
		t.Errorf("after reparse: %v, want forwarded 1.2 and control 2", got)
	}
	var structureAfter []byte
	if err := db.Pool().QueryRow(ctx, `SELECT bodystructure FROM messages WHERE subject = 'forwarded'`).Scan(&structureAfter); err != nil {
		t.Fatal(err)
	}
	if !jsonEquivalent(structureBefore, structureAfter) {
		t.Errorf("the structure changed:\n%s\n%s", structureBefore, structureAfter)
	}

	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("second reparse: %v", err)
	}
	if st.Rewritten != 0 || st.Unchanged != 2 {
		t.Errorf("second reparse: %+v, want nothing left to rewrite", st)
	}

	if !sameAttachmentParts([]string{"2", "1.2"}, []ingest.Attachment{{PartNumber: "1.2"}, {PartNumber: "2"}}) ||
		sameAttachmentParts([]string{"2", "2"}, []ingest.Attachment{{PartNumber: "2"}, {PartNumber: "1.2"}}) ||
		sameAttachmentParts(nil, []ingest.Attachment{{PartNumber: "1"}}) ||
		!sameAttachmentParts(nil, nil) {
		t.Error("sameAttachmentParts does not compare part numbers as multisets")
	}
}
