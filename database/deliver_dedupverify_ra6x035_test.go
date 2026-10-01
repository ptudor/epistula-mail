package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDuplicateDeliveryVerifiesItsStoredBlob is the RA6X-035 delivery-boundary
// regression.
//
// Postfix retries a delivery whenever the LDA exits non-zero, and the LDA
// answers a retry by checking (folder, raw_sha256) in the database and exiting
// EX_OK. A row saying "we already have this" is not the same as having it: if
// the raw blob has been lost or corrupted since, acknowledging the retry
// discards the correct bytes on stdin — the only copy left — and leaves an
// acknowledged message nobody can read.
func TestDuplicateDeliveryVerifiesItsStoredBlob(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	defer teardown()

	raw := "From: sender@ra6x008.invalid\r\nSubject: duplicate probe\r\n\r\nbody\r\n"

	// First delivery.
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatalf("first delivery exit=%d, want EX_OK", code)
	}
	blobPath := onlyRawBlobPath(t, cfg.Storage.Root)

	// An identical retry with the blob intact is the ordinary duplicate: it
	// must be acknowledged without storing a second row.
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatalf("healthy duplicate exit=%d, want EX_OK", code)
	}

	// Now lose the blob and retry again. The delivery must NOT take the
	// duplicate shortcut: it has the bytes and must re-store them.
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatalf("duplicate with a missing blob exit=%d, want EX_OK", code)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("the retried bytes were not re-stored: %v", err)
	}

	// And with a corrupt blob: same requirement, and the file must end up
	// holding the correct bytes.
	if err := os.WriteFile(blobPath, []byte("corrupt"), 0o640); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatalf("duplicate with a corrupt blob exit=%d, want EX_OK", code)
	}
	onDisk, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(onDisk) != raw {
		t.Errorf("the corrupt blob was not repaired: %q", onDisk)
	}

	// Throughout, exactly one message row: the dedup contract is intact.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n := countMessageRows(t, ctx, dsnOf(t, cfg)); n != 1 {
		t.Fatalf("%d message rows after four deliveries of one message; want 1", n)
	}
}

// onlyRawBlobPath returns the single raw blob under root, failing if there is
// not exactly one.
func onlyRawBlobPath(t *testing.T, root string) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".eml" {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d raw blobs under %s, want 1", len(found), root)
	}
	return found[0]
}

// dsnOf recovers the privileged DSN the fixture configured.
func dsnOf(t *testing.T, cfg *Config) string {
	t.Helper()
	return cfg.Postgres.DSN
}
