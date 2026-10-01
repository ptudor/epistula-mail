package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/ptudor/epistula-mail/database/blob"
)

// blobPathFor locates the on-disk raw blob backing a message row.
func blobPathFor(t *testing.T, f *apiFixture, id int64) string {
	t.Helper()
	var shaHex string
	var bucketDate any
	if err := f.pool.QueryRow(context.Background(),
		`SELECT encode(raw_sha256, 'hex'), raw_blob_date FROM messages WHERE id = $1`, id,
	).Scan(&shaHex, &bucketDate); err != nil {
		t.Fatalf("locate blob: %v", err)
	}
	var ref messageRef
	if err := f.pool.QueryRow(context.Background(),
		`SELECT mb.name, m.raw_blob_date FROM messages m
		   JOIN folders f ON f.id = m.folder_id
		   JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE m.id = $1`, id,
	).Scan(&ref.Mailbox, &ref.RawBlobDate); err != nil {
		t.Fatalf("locate tenant: %v", err)
	}
	tenant, err := blob.ParseTenant(ref.Mailbox)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := hex.DecodeString(shaHex); err != nil {
		t.Fatalf("sha: %v", err)
	}
	p, err := f.srv.store.PathFor(blob.KindRaw, tenant, blob.BucketFromTime(ref.RawBlobDate), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	return p
}

// TestRawRefusesWhenDiskDisagreesWithDB is the RO5X-005 regression.
//
// Content-Length is declared from messages.raw_size while the bytes come off
// disk. When they disagree the response is malformed: short reads hang the
// client until its own timeout, long reads are silently truncated by
// net/http and re-parsed by the consumer as a different message. The handler
// must refuse with a 500 before writing any header.
func TestRawRefusesWhenDiskDisagreesWithDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string, orig []byte)
	}{
		{
			name: "disk shorter than DB",
			mutate: func(t *testing.T, path string, orig []byte) {
				if err := os.WriteFile(path, orig[:len(orig)-1], 0o640); err != nil {
					t.Fatalf("truncate: %v", err)
				}
			},
		},
		{
			name: "disk longer than DB",
			mutate: func(t *testing.T, path string, orig []byte) {
				if err := os.WriteFile(path, append(append([]byte{}, orig...), 'X'), 0o640); err != nil {
					t.Fatalf("extend: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			id := f.aliceMsgIDs[0]

			// Sanity: the untouched blob streams fine.
			resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/raw", id), f.classifierToken, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("baseline raw status = %d, want 200", resp.StatusCode)
			}
			resp.Body.Close()

			path := blobPathFor(t, f, id)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read blob: %v", err)
			}
			// The blob store writes 0440/0640 files; make it writable to corrupt it.
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			tc.mutate(t, path, orig)

			resp = f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/raw", id), f.classifierToken, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500 on a size mismatch", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			// No message bytes must have escaped.
			body, _ := io.ReadAll(resp.Body)
			if len(body) > 0 && string(body[:min(len(body), 8)]) == "Subject:" {
				t.Errorf("raw message bytes were written despite the mismatch: %q", body)
			}
		})
	}
}

// TestSafeShortSHA covers the guard the RO5X-005 size-mismatch log line uses:
// a stored sha256 shorter than 16 characters is logged whole rather than
// panicking on an unguarded slice (RO5X-029).
func TestSafeShortSHA(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"0123456789abcdef", "0123456789abcdef"},
		{"0123456789abcdef0123", "0123456789abcdef"},
	} {
		if got := safeShortSHA(tc.in); got != tc.want {
			t.Errorf("safeShortSHA(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
