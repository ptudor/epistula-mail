package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// duplicateAttachmentsQuery counts, per mailbox, the messages that carry the
// OPS-006 duplicates and the surplus rows. It only reads. An operator can run
// it to size the repair; this test runs it verbatim.
const duplicateAttachmentsQuery = `
SELECT mb.name AS mailbox,
       count(DISTINCT d.message_id) AS messages,
       sum(d.n - 1) AS extra_rows
  FROM (SELECT message_id, part_number, count(*) AS n
          FROM attachments
         GROUP BY message_id, part_number
        HAVING count(*) > 1) d
  JOIN messages m ON m.id = d.message_id
  JOIN folders f ON f.id = m.folder_id
  JOIN mailboxes mb ON mb.id = f.mailbox_id
 GROUP BY mb.name
 ORDER BY mb.name`

// TestReparseRemovesDuplicateAttachmentRows is the OPS-006 back-fill. Before
// the fix, ingest wrote two identical rows for an attached text part that was
// neither text/plain nor text/html. The duplicates repeat a part number, so
// the count of rows differs from a fresh parse's, and sameAttachmentParts,
// which compares part numbers as multisets, reports the message as changed.
// This proves that end to end on stored rows rather than assuming it.
func TestReparseRemovesDuplicateAttachmentRows(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gcMustMailbox(t, ctx, db, "dupatt")

	csv := "From: a@ops006.invalid\r\nSubject: csv\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nthe report\r\n" +
		"--b\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=\"report.csv\"\r\n\r\na,b\r\n1,2\r\n" +
		"--b--\r\n"
	invite := "From: a@ops006.invalid\r\nSubject: invite\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain\r\n\r\njoin us\r\n" +
		"--alt\r\nContent-Type: text/html\r\n\r\n<p>join us</p>\r\n" +
		"--alt\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
		"--alt--\r\n\r\n" +
		"--b\r\nContent-Type: text/calendar; method=REQUEST\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nnotes\r\n" +
		"--b--\r\n"
	control := "From: a@ops006.invalid\r\nSubject: control\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\n%PDF-1.4\r\n" +
		"--b--\r\n"

	root, storageRoot := t.TempDir(), t.TempDir()
	writeMaildirMessage(t, root, "0001", csv)
	writeMaildirMessage(t, root, "0002", invite)
	writeMaildirMessage(t, root, "0003", control)
	cfg := writeRenameConfig(t, dsn, storageRoot)
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "dupatt"}); code != EX_OK {
		t.Fatalf("import exit %d", code)
	}

	// Every column of every attachment row, in a stable order.
	type row struct {
		Subject, Part, Filename, ContentType, Disposition, SHA string
		Size                                                   int64
		BlobDate                                               time.Time
	}
	rows := func() []row {
		t.Helper()
		r, err := db.Pool().Query(ctx, `
			SELECT m.subject, a.part_number, COALESCE(a.filename, ''), a.content_type,
			       COALESCE(a.disposition, ''), encode(a.sha256, 'hex'), a.size_bytes, a.blob_date
			  FROM attachments a JOIN messages m ON m.id = a.message_id
			 ORDER BY m.subject, a.part_number, a.id`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out []row
		for r.Next() {
			var x row
			if err := r.Scan(&x.Subject, &x.Part, &x.Filename, &x.ContentType, &x.Disposition, &x.SHA, &x.Size, &x.BlobDate); err != nil {
				t.Fatal(err)
			}
			out = append(out, x)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	type dupCount struct {
		Mailbox          string
		Messages, Extras int64
	}
	duplicates := func() []dupCount {
		t.Helper()
		r, err := db.Pool().Query(ctx, duplicateAttachmentsQuery)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out []dupCount
		for r.Next() {
			var d dupCount
			if err := r.Scan(&d.Mailbox, &d.Messages, &d.Extras); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	structures := func() map[string]string {
		t.Helper()
		r, err := db.Pool().Query(ctx, `SELECT subject, bodystructure::text FROM messages`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]string{}
		for r.Next() {
			var s, bs string
			if err := r.Scan(&s, &bs); err != nil {
				t.Fatal(err)
			}
			out[s] = bs
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	imported := rows()
	var parts []string
	for _, x := range imported {
		parts = append(parts, x.Subject+" "+x.Part+" "+x.ContentType+" "+x.Filename)
	}
	wantParts := []string{
		"control 2 application/pdf a.pdf",
		"csv 2 text/csv report.csv",
		"invite 1.3 text/calendar ",
		"invite 2 text/calendar invite.ics",
		"invite 3 text/plain notes.txt",
	}
	if !reflect.DeepEqual(parts, wantParts) {
		t.Fatalf("imported rows:\n%q\nwant:\n%q", parts, wantParts)
	}
	if got := duplicates(); len(got) != 0 {
		t.Fatalf("the fixed ingest wrote duplicates: %+v", got)
	}

	// The rows the pre-fix ingest wrote: each attached text part that is not
	// body text a second time, identical to the first.
	tag, err := db.Pool().Exec(ctx, `
		INSERT INTO attachments (message_id, part_number, filename, content_type,
		                         content_id, disposition, size_bytes, sha256, blob_date)
		SELECT a.message_id, a.part_number, a.filename, a.content_type,
		       a.content_id, a.disposition, a.size_bytes, a.sha256, a.blob_date
		  FROM attachments a JOIN messages m ON m.id = a.message_id
		 WHERE (m.subject, a.part_number) IN (('csv', '2'), ('invite', '2'))`)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 2 {
		t.Fatalf("seeded %d duplicate rows, want 2", tag.RowsAffected())
	}
	if got, want := duplicates(), []dupCount{{"dupatt", 2, 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("count query = %+v, want %+v", got, want)
	}
	seeded := rows()
	structureBefore := structures()

	store := blob.NewStore(storageRoot)
	parser := ingest.NewSalvaging(ingest.DefaultLimits())
	st, err := reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if st.Rewritten != 2 || st.Unchanged != 1 {
		t.Errorf("dry run: %+v, want 2 to rewrite and 1 unchanged", st)
	}
	if got := rows(); !reflect.DeepEqual(got, seeded) {
		t.Fatalf("the dry run changed attachment rows:\n%+v\nwant:\n%+v", got, seeded)
	}

	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if st.Rewritten != 2 || st.Unchanged != 1 {
		t.Errorf("reparse: %+v, want 2 rewritten and 1 unchanged", st)
	}
	// Exactly the rows a correct import wrote: same parts, names, types,
	// sizes, content addresses and buckets.
	if got := rows(); !reflect.DeepEqual(got, imported) {
		t.Errorf("after reparse:\n%+v\nwant the imported rows:\n%+v", got, imported)
	}
	if got := duplicates(); len(got) != 0 {
		t.Errorf("count query after reparse = %+v, want no rows", got)
	}
	if got := structures(); !reflect.DeepEqual(got, structureBefore) {
		t.Errorf("reparse changed a structure:\n%v\nwant:\n%v", got, structureBefore)
	}

	st, err = reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10})
	if err != nil {
		t.Fatalf("second reparse: %v", err)
	}
	if st.Rewritten != 0 || st.Unchanged != 3 {
		t.Errorf("second reparse: %+v, want nothing left to rewrite", st)
	}

	if sameAttachmentParts([]string{"2", "2"}, []ingest.Attachment{{PartNumber: "2"}}) ||
		sameAttachmentParts([]string{"2", "2", "1.3"}, []ingest.Attachment{{PartNumber: "1.3"}, {PartNumber: "2"}}) {
		t.Error("sameAttachmentParts accepts a repeated part number")
	}
}
