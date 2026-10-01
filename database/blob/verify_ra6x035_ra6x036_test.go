package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func verifyStore(t *testing.T) (*Store, Tenant, Bucket) {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, Tenant("alice"), BucketFromTime(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
}

func writeVerifyBlob(t *testing.T, s *Store, kind Kind, tenant Tenant, bucket Bucket, data []byte) (string, string) {
	t.Helper()
	w, err := s.NewWriter(kind, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := s.PathFor(kind, tenant, bucket, sha)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	return sha, p
}

// TestCorruptBlobIsRepairedOnRedelivery is the RA6X-035 regression.
//
// A file at the expected content-addressed path was accepted as authoritative
// on sight. A redelivery of the same message — which has the CORRECT bytes in
// hand — saw EEXIST, set deduped=true, deleted its own correct temporary file
// and acknowledged the message. The last good copy was destroyed by the code
// that could have restored it.
func TestCorruptBlobIsRepairedOnRedelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(path string, good []byte)
	}{
		{
			name: "truncated",
			corrupt: func(path string, good []byte) {
				if err := os.WriteFile(path, good[:len(good)/2], 0o640); err != nil {
					t.Fatalf("truncate: %v", err)
				}
			},
		},
		{
			// The case only hashing can catch: same length, different bytes.
			name: "equal-size alteration",
			corrupt: func(path string, good []byte) {
				bad := append([]byte(nil), good...)
				bad[0] ^= 0xff
				if err := os.WriteFile(path, bad, 0o640); err != nil {
					t.Fatalf("alter: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tenant, bucket := verifyStore(t)
			good := bytes.Repeat([]byte("the original message bytes\n"), 40)

			sha, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, good)
			tc.corrupt(path, good)

			// The redelivery.
			w, err := s.NewWriter(KindRaw, tenant, bucket)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			if _, err := w.Write(good); err != nil {
				t.Fatalf("Write: %v", err)
			}
			gotSHA, _, deduped, err := w.Close()
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
			if gotSHA != sha {
				t.Fatalf("sha = %s, want %s", gotSHA, sha)
			}
			if deduped {
				t.Error("a corrupt existing blob was reported as a dedup hit")
			}

			// The store must now hold the correct bytes.
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read repaired blob: %v", err)
			}
			if !bytes.Equal(onDisk, good) {
				t.Errorf("blob was not repaired: have %d bytes, want %d", len(onDisk), len(good))
			}
			sum := sha256.Sum256(onDisk)
			if hex.EncodeToString(sum[:]) != sha {
				t.Error("the repaired blob does not match its content address")
			}
		})
	}
}

// TestHealthyDedupStillDedups pins that the ordinary case is unchanged: an
// identical redelivery reuses the file and reports a dedup hit.
func TestHealthyDedupStillDedups(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	data := []byte("a perfectly good message")

	_, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, data)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	w, _ := s.NewWriter(KindRaw, tenant, bucket)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, _, deduped, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !deduped {
		t.Error("an identical redelivery was not reported as a dedup hit")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a healthy blob was needlessly rewritten")
	}
}

// TestEnsureContentRepairsACorruptBlob pins the other reuse boundary:
// EnsureContent exists to re-verify a blob survived a concurrent sweep, and
// checked only existence — so the transaction committed a row pointing at
// content the store could not serve.
func TestEnsureContentRepairsACorruptBlob(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	data := []byte("content the transaction is about to reference")
	sha, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, data)

	// Equal-size corruption, which existence and size checks both miss.
	bad := append([]byte(nil), data...)
	bad[3] ^= 0xff
	if err := os.WriteFile(path, bad, 0o640); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	rewritten, err := s.EnsureContent(KindRaw, tenant, bucket, sha, data)
	if err != nil {
		t.Fatalf("EnsureContent: %v", err)
	}
	if !rewritten {
		t.Error("EnsureContent accepted a blob that does not match its content address")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(onDisk, data) {
		t.Error("EnsureContent did not restore the correct bytes")
	}

	// A healthy blob is left alone.
	rewritten, err = s.EnsureContent(KindRaw, tenant, bucket, sha, data)
	if err != nil {
		t.Fatalf("EnsureContent (healthy): %v", err)
	}
	if rewritten {
		t.Error("EnsureContent rewrote a healthy blob")
	}
}

// TestVerifySizeModeCatchesTruncation pins the cheaper policy: size-only
// verification still catches the common corruption.
func TestVerifySizeModeCatchesTruncation(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	s.SetVerifyMode(VerifySize)
	data := bytes.Repeat([]byte("x"), 200)
	sha, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, data)

	if err := os.WriteFile(path, data[:10], 0o640); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	rewritten, err := s.EnsureContent(KindRaw, tenant, bucket, sha, data)
	if err != nil {
		t.Fatalf("EnsureContent: %v", err)
	}
	if !rewritten {
		t.Error("size verification missed a truncated blob")
	}
}

// TestDurabilityMarkerProvesAncestry is the RA6X-036 regression.
//
// ensureDir's fast path returned as soon as the leaf directory existed, so one
// writer could observe directories another had created but not yet fsynced,
// sync only the leaf, and commit its row — and if the creator exited first, a
// power loss could erase the chain holding the acknowledged blob. A
// process-local mutex cannot help, because the LDA and the IMAP server are
// separate processes.
//
// The marker's presence is the cross-process proof. This asserts the protocol:
// a bucket that has been written to carries one, a writer that finds one takes
// the fast path, and a writer that does NOT find one re-establishes durability
// rather than assuming it.
func TestDurabilityMarkerProvesAncestry(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	_, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, []byte("first blob"))

	shard := filepath.Dir(path)
	marker, ok := markerPathFor(s.root, shard)
	if !ok {
		t.Fatalf("no marker path derived for %s", shard)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a written bucket has no durability marker: %v", err)
	}

	// A SECOND process — no memoized state — must accept the marker as proof.
	peer := NewStore(s.root)
	if err := peer.ensureDir(shard); err != nil {
		t.Fatalf("peer ensureDir: %v", err)
	}

	// With the marker gone (a crashed creator, or a pruned bucket), a writer
	// must re-establish and re-publish durability rather than assume it.
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	third := NewStore(s.root)
	if err := third.ensureDir(shard); err != nil {
		t.Fatalf("third ensureDir: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a writer that found no proof did not publish one: %v", err)
	}
}

// TestDurabilityMarkerIsRemovableWhenAloneOnly pins the GC interaction: an
// emptied bucket can be pruned, and one that still holds a blob cannot.
func TestDurabilityMarkerIsRemovableWhenAloneOnly(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	_, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, []byte("blob"))
	shard := filepath.Dir(path)
	marker, _ := markerPathFor(s.root, shard)
	bucketDir := filepath.Dir(marker)

	if RemoveDurabilityMarkerIfOnlyEntry(bucketDir) {
		t.Fatal("the marker was removed from a bucket that still holds shard directories")
	}

	// Empty the bucket the way a sweep does.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	for d := shard; d != bucketDir; d = filepath.Dir(d) {
		if err := os.Remove(d); err != nil {
			t.Fatalf("remove %s: %v", d, err)
		}
	}
	if !RemoveDurabilityMarkerIfOnlyEntry(bucketDir) {
		t.Fatal("the marker was not removed from an otherwise empty bucket")
	}
	if err := os.Remove(bucketDir); err != nil {
		t.Fatalf("the emptied bucket could not be pruned: %v", err)
	}
}
