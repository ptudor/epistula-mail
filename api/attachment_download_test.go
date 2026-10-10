package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
)

// attachmentBlobDate is the bucket the fixture's attachment blobs are written
// under, deliberately different from the parent message's bucket: reads must
// follow attachments.blob_date, not messages.raw_blob_date.
var attachmentBlobDate = time.Date(2014, 3, 12, 0, 0, 0, 0, time.UTC)

// insertAttachment writes data to the tenant's att blob tree and its row to
// PG, the way ingest stores a decoded MIME part.
func (f *apiFixture) insertAttachment(ctx context.Context, tenant blob.Tenant, messageID int64, part string, filename *string, contentType string, data []byte) string {
	f.t.Helper()
	w, err := f.srv.store.NewWriter(blob.KindAttachment, tenant, blob.BucketFromTime(attachmentBlobDate))
	if err != nil {
		f.t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		f.t.Fatalf("blob write: %v", err)
	}
	shaHex, _, _, err := w.Close()
	if err != nil {
		f.t.Fatalf("blob close: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO attachments (message_id, part_number, filename, content_type, disposition, size_bytes, sha256, blob_date)
		VALUES ($1, $2, $3, $4, 'attachment', $5, decode($6, 'hex'), $7)`,
		messageID, part, filename, contentType, int64(len(data)), shaHex, attachmentBlobDate,
	); err != nil {
		f.t.Fatalf("insert attachment %s: %v", part, err)
	}
	return shaHex
}

func attachmentPath(id int64, part string) string {
	return fmt.Sprintf("/v1/messages/%d/attachments/%s", id, part)
}

func TestAPIAttachmentDownload(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := f.aliceMsgIDs[1]
	pdf := []byte("%PDF-1.4\n% Digi-Key invoice fixture\n%%EOF\n")
	name := "Rechnung Größe \"März\".pdf"
	f.insertAttachment(ctx, "alice", id, "2", &name, "application/pdf", pdf)
	png := []byte("\x89PNG\r\n\x1a\nnot really an image")
	f.insertAttachment(ctx, "alice", id, "3.1", nil, "image/png", png)

	resp := f.do(http.MethodGet, attachmentPath(id, "2"), f.aliceContentToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, b)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, pdf) {
		t.Errorf("body = %q, want the stored attachment bytes", got)
	}
	for header, want := range map[string]string{
		"Content-Type":            "application/pdf",
		"Content-Length":          fmt.Sprint(len(pdf)),
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
	} {
		if v := resp.Header.Get(header); v != want {
			t.Errorf("%s = %q, want %q", header, v, want)
		}
	}
	disposition, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("Content-Disposition %q does not parse: %v", resp.Header.Get("Content-Disposition"), err)
	}
	if disposition != "attachment" || params["filename"] != name {
		t.Errorf("Content-Disposition = %q %v, want attachment with filename %q", disposition, params, name)
	}

	// A nested part with no filename: a bare attachment disposition.
	resp2 := f.do(http.MethodGet, attachmentPath(id, "3.1"), f.classifierToken, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("nested part status = %d, want 200", resp2.StatusCode)
	}
	got2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(got2, png) {
		t.Errorf("nested part body = %q, want the stored bytes", got2)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("nested part Content-Type = %q, want image/png", ct)
	}
	if cd := resp2.Header.Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("unnamed part Content-Disposition = %q, want attachment", cd)
	}
}

// TestAPIAttachmentAccess pins the gates: read_content is required, the
// message's mailbox must be in scope before the part is even looked up (403,
// never a 404 that reveals whether the part exists), and a part that is absent
// or malformed is a 404 problem.
func TestAPIAttachmentAccess(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := f.aliceMsgIDs[0]
	f.insertAttachment(ctx, "alice", id, "2", nil, "application/pdf", []byte("%PDF-1.4\n"))

	for _, tc := range []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"metadata token lacks read_content", attachmentPath(id, "2"), f.metaToken, http.StatusForbidden},
		{"out-of-scope message", attachmentPath(id, "2"), f.bobToken, http.StatusForbidden},
		{"out-of-scope message, absent part", attachmentPath(id, "9"), f.bobToken, http.StatusForbidden},
		{"absent part", attachmentPath(id, "9"), f.aliceContentToken, http.StatusNotFound},
		{"absent message", attachmentPath(999999, "2"), f.classifierToken, http.StatusNotFound},
		{"trailing dot", attachmentPath(id, "2."), f.aliceContentToken, http.StatusNotFound},
		{"not a part path", attachmentPath(id, "abc"), f.aliceContentToken, http.StatusNotFound},
		{"escaped dot-dot", attachmentPath(id, "%2E%2E"), f.aliceContentToken, http.StatusNotFound},
		{"over-long", attachmentPath(id, strings.Repeat("1.", maxPartNumberLen)+"1"), f.aliceContentToken, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantProblem(t, f.do(http.MethodGet, tc.path, tc.token, nil), tc.status)
		})
	}
}

// TestAPIAttachmentRefusesBadBlob applies the RO5X-005 rule to attachments: a
// blob whose size disagrees with the row, or that is missing, is a 500 before
// any attachment byte is written.
func TestAPIAttachmentRefusesBadBlob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"disk longer than DB", func(t *testing.T, path string) {
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read blob: %v", err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if err := os.WriteFile(path, append(orig, 'X'), 0o640); err != nil {
				t.Fatalf("extend: %v", err)
			}
		}},
		{"blob missing", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			id := f.aliceMsgIDs[0]
			secret := []byte("SECRET-ATTACHMENT-BYTES")
			shaHex := f.insertAttachment(ctx, "alice", id, "2", nil, "application/octet-stream", secret)

			path, err := f.srv.store.PathFor(blob.KindAttachment, "alice", blob.BucketFromTime(attachmentBlobDate), shaHex)
			if err != nil {
				t.Fatalf("PathFor: %v", err)
			}
			tc.mutate(t, path)

			resp := f.do(http.MethodGet, attachmentPath(id, "2"), f.aliceContentToken, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			wantProblem(t, resp, http.StatusInternalServerError)
			if bytes.Contains(body, secret) {
				t.Errorf("attachment bytes were written despite the bad blob: %q", body)
			}
		})
	}
}

func TestValidPartNumber(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"1", true},
		{"2", true},
		{"1.2.3", true},
		{"10.20", true},
		{"", false},
		{".", false},
		{"..", false},
		{"1.", false},
		{".1", false},
		{"1..2", false},
		{"1/2", false},
		{"1.a", false},
		{"TEXT", false},
		{"-1", false},
		{strings.Repeat("1", maxPartNumberLen), true},
		{strings.Repeat("1", maxPartNumberLen+1), false},
	} {
		if got := validPartNumber(tc.in); got != tc.want {
			t.Errorf("validPartNumber(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAttachmentMediaType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"application/pdf", "application/pdf"},
		{"Image/PNG", "image/png"},
		{"text/html", "text/html"},
		{"", "application/octet-stream"},
		{"pdf", "application/octet-stream"},
		{"application/", "application/octet-stream"},
		{"application/pdf\r\nSet-Cookie: a=b", "application/octet-stream"},
		{"application/pdf; name=\"x.pdf\"", "application/pdf"},
	} {
		if got := attachmentMediaType(tc.in); got != tc.want {
			t.Errorf("attachmentMediaType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAttachmentDisposition: the sender controls the filename, so whatever it
// holds must come back out of the header intact and must never break the
// header itself.
func TestAttachmentDisposition(t *testing.T) {
	str := func(s string) *string { return &s }
	if got := attachmentDisposition(nil); got != "attachment" {
		t.Errorf("nil filename = %q, want attachment", got)
	}
	if got := attachmentDisposition(str("")); got != "attachment" {
		t.Errorf("empty filename = %q, want attachment", got)
	}
	for _, name := range []string{
		"invoice.pdf",
		"Digi-Key Invoice 2014.pdf",
		"quote\"and\\backslash.pdf",
		"semi;colon=equals.pdf",
		"Größe.pdf",
		"line\r\nSet-Cookie: a=b.pdf",
		"../../etc/passwd",
	} {
		v := attachmentDisposition(&name)
		if strings.ContainsAny(v, "\r\n") {
			t.Errorf("filename %q produced a header with a line break: %q", name, v)
		}
		disposition, params, err := mime.ParseMediaType(v)
		if err != nil {
			t.Errorf("filename %q produced an unparsable header %q: %v", name, v, err)
			continue
		}
		if disposition != "attachment" || params["filename"] != name || len(params) != 1 {
			t.Errorf("filename %q round-tripped as %q %v", name, disposition, params)
		}
	}
}
