package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testTenant / otherTenant are canonical mailbox-name tenants used across the
// blob tests.
const (
	testTenant  Tenant = "alice"
	otherTenant Tenant = "bob"
)

func setupStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

// fixedBucket returns a single date so tests don't drift on a midnight UTC
// rollover. The bucket is otherwise arbitrary.
func fixedBucket() Bucket {
	return BucketFromTime(time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC))
}

func TestValidateSHA(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", false},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", false},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8555", false},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g", false},
		{"", false},
		{"../etc/passwd", false},
		{"/abs/path", false},
	}
	for _, c := range cases {
		err := ValidateSHA(c.s)
		got := err == nil
		if got != c.want {
			t.Errorf("ValidateSHA(%q) = %v, want %v (err=%v)", c.s, got, c.want, err)
		}
	}
}

func TestParseTenant(t *testing.T) {
	cases := []struct {
		s  string
		ok bool
	}{
		{"alice", true},
		{"a", true},
		{"user.name_1-2", true},
		{"a0123456789", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false}, // too long
		{"", false},
		{".", false},
		{"..", false},
		{".hidden", false},       // leading dot
		{"_leading", false},      // leading underscore not in [a-z0-9]
		{"-leading", false},      // leading hyphen
		{"Alice", false},         // uppercase
		{"a/b", false},           // slash
		{"../etc", false},        // traversal
		{"a b", false},           // space
		{"user@host", false},     // '@'
		{"john.doe+test", false}, // '+'
		{"café", false},          // non-ASCII
	}
	for _, c := range cases {
		ten, err := ParseTenant(c.s)
		gotOK := err == nil
		if gotOK != c.ok {
			t.Errorf("ParseTenant(%q) ok=%v, want %v (err=%v)", c.s, gotOK, c.ok, err)
		}
		if c.ok && string(ten) != c.s {
			t.Errorf("ParseTenant(%q) must be identity, got %q", c.s, ten)
		}
	}
}

func TestBucketParse(t *testing.T) {
	cases := []struct {
		s   string
		ok  bool
		out Bucket
	}{
		{"2026/05/18", true, "2026/05/18"},
		{"1970/01/01", true, "1970/01/01"},
		{"unknown", false, ""},
		{"2026/13/01", false, ""},
		{"2026/02/30", false, ""},
		{"26/05/18", false, ""},
		{"2026-05-18", false, ""},
		{"", false, ""},
		{"../etc", false, ""},
		{"2026/05/18/", false, ""},
	}
	for _, c := range cases {
		b, err := ParseBucket(c.s)
		gotOK := err == nil
		if gotOK != c.ok {
			t.Errorf("ParseBucket(%q) ok=%v, want %v (err=%v)", c.s, gotOK, c.ok, err)
		}
		if c.ok && b != c.out {
			t.Errorf("ParseBucket(%q) = %q, want %q", c.s, b, c.out)
		}
	}
}

func TestBucketFromTime(t *testing.T) {
	if got := BucketFromTime(time.Date(2026, 5, 18, 23, 59, 59, 0, time.UTC)); got != "2026/05/18" {
		t.Errorf("BucketFromTime UTC: got %q, want 2026/05/18", got)
	}
	if got := BucketFromTime(time.Time{}); got != BucketUnknown() {
		t.Errorf("BucketFromTime zero: got %q, want %q", got, BucketUnknown())
	}
	if got := BucketUnknown(); got != "1970/01/01" {
		t.Errorf("BucketUnknown: got %q, want %q", got, "1970/01/01")
	}
}

func TestPathConstructionRejectsTraversal(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	bad := []string{
		"../../../etc/passwd",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85/../malicious",
		"/etc/passwd",
		"..",
	}
	for _, p := range bad {
		if _, err := s.PathFor(KindRaw, testTenant, bk, p); !errors.Is(err, ErrInvalidSHA) {
			t.Errorf("PathFor(KindRaw, %q) = %v, want ErrInvalidSHA", p, err)
		}
	}
}

func TestPathConstructionRejectsBadBucket(t *testing.T) {
	s := setupStore(t)
	hx := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	for _, b := range []Bucket{"", "2026/13/01", "../malicious"} {
		if _, err := s.PathFor(KindRaw, testTenant, b, hx); !errors.Is(err, ErrInvalidBucket) {
			t.Errorf("PathFor with bucket=%q: err = %v, want ErrInvalidBucket", b, err)
		}
	}
}

func TestPathConstructionRejectsBadTenant(t *testing.T) {
	s := setupStore(t)
	hx := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	bk := fixedBucket()
	for _, ten := range []Tenant{"", "..", "../evil", "Alice", "a/b", ".hidden"} {
		if _, err := s.PathFor(KindRaw, ten, bk, hx); !errors.Is(err, ErrInvalidTenant) {
			t.Errorf("PathFor with tenant=%q: err = %v, want ErrInvalidTenant", ten, err)
		}
		if _, err := s.NewWriter(KindRaw, ten, bk); !errors.Is(err, ErrInvalidTenant) {
			t.Errorf("NewWriter with tenant=%q: err = %v, want ErrInvalidTenant", ten, err)
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()

	w, err := s.NewWriter(KindRaw, testTenant, bk)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	payload := []byte("hello, epistula-database\n")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, size, deduped, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if deduped {
		t.Error("first write should not be deduped")
	}
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])
	if sha != wantSHA {
		t.Errorf("sha = %q, want %q", sha, wantSHA)
	}

	exists, err := s.Exists(KindRaw, testTenant, bk, sha)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("blob does not exist after Close")
	}

	r, err := s.Open(KindRaw, testTenant, bk, sha)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, payload)
	}
}

func TestKindsSeparate(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	payload := []byte("shared content")

	w1, _ := s.NewWriter(KindRaw, testTenant, bk)
	w1.Write(payload)
	sha1, _, _, err := w1.Close()
	if err != nil {
		t.Fatalf("raw Close: %v", err)
	}

	w2, _ := s.NewWriter(KindAttachment, testTenant, bk)
	w2.Write(payload)
	sha2, _, deduped, err := w2.Close()
	if err != nil {
		t.Fatalf("att Close: %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("same content should hash the same: %q vs %q", sha1, sha2)
	}
	if deduped {
		t.Error("att should not dedup raw — they're separate trees")
	}

	if _, err := s.Open(KindRaw, testTenant, bk, sha1); err != nil {
		t.Errorf("Open KindRaw: %v", err)
	}
	if _, err := s.Open(KindAttachment, testTenant, bk, sha2); err != nil {
		t.Errorf("Open KindAttachment: %v", err)
	}
}

func TestDedupOnDuplicateWriteSameBucket(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	payload := []byte("same content")

	w1, _ := s.NewWriter(KindRaw, testTenant, bk)
	w1.Write(payload)
	sha1, _, dedup1, err := w1.Close()
	if err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if dedup1 {
		t.Error("first write should not be deduped")
	}

	w2, _ := s.NewWriter(KindRaw, testTenant, bk)
	w2.Write(payload)
	sha2, _, dedup2, err := w2.Close()
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("sha mismatch: %q vs %q", sha1, sha2)
	}
	if !dedup2 {
		t.Error("second write into the same tenant+bucket should be deduped")
	}
}

func TestNoDedupAcrossBuckets(t *testing.T) {
	s := setupStore(t)
	payload := []byte("same content, different days")

	day1 := BucketFromTime(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	day2 := BucketFromTime(time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))

	w1, _ := s.NewWriter(KindRaw, testTenant, day1)
	w1.Write(payload)
	sha1, _, dedup1, err := w1.Close()
	if err != nil {
		t.Fatalf("day1 Close: %v", err)
	}
	if dedup1 {
		t.Error("first write should not be deduped")
	}

	w2, _ := s.NewWriter(KindRaw, testTenant, day2)
	w2.Write(payload)
	sha2, _, dedup2, err := w2.Close()
	if err != nil {
		t.Fatalf("day2 Close: %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("sha drift across buckets: %q vs %q", sha1, sha2)
	}
	if dedup2 {
		t.Error("write into a different bucket must NOT dedup")
	}

	// Both blobs are independently readable in their own bucket.
	if _, err := s.Open(KindRaw, testTenant, day1, sha1); err != nil {
		t.Errorf("Open day1: %v", err)
	}
	if _, err := s.Open(KindRaw, testTenant, day2, sha2); err != nil {
		t.Errorf("Open day2: %v", err)
	}
}

// TestNoDedupAcrossTenants is the core isolation contract: identical bytes
// written for two mailboxes produce two independent on-disk files, neither
// deduping against the other, each visible only in its own tenant subtree.
func TestNoDedupAcrossTenants(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	payload := []byte("a newsletter sent to two different mailboxes")

	w1, _ := s.NewWriter(KindRaw, testTenant, bk)
	w1.Write(payload)
	sha1, _, dedup1, err := w1.Close()
	if err != nil {
		t.Fatalf("alice Close: %v", err)
	}
	if dedup1 {
		t.Error("alice's write should not be deduped")
	}

	w2, _ := s.NewWriter(KindRaw, otherTenant, bk)
	w2.Write(payload)
	sha2, _, dedup2, err := w2.Close()
	if err != nil {
		t.Fatalf("bob Close: %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("content hash should match across tenants: %q vs %q", sha1, sha2)
	}
	if dedup2 {
		t.Error("bob's identical write must NOT dedup against alice — tenants are isolated")
	}

	// Each tenant sees only its own copy.
	if ok, _ := s.Exists(KindRaw, testTenant, bk, sha1); !ok {
		t.Error("alice's blob missing")
	}
	if ok, _ := s.Exists(KindRaw, otherTenant, bk, sha2); !ok {
		t.Error("bob's blob missing")
	}

	// They are physically distinct files.
	pa, _ := s.PathFor(KindRaw, testTenant, bk, sha1)
	pb, _ := s.PathFor(KindRaw, otherTenant, bk, sha2)
	if pa == pb {
		t.Fatalf("tenant paths collided: %q", pa)
	}
	if !strings.Contains(pa, string(testTenant)) || !strings.Contains(pb, string(otherTenant)) {
		t.Errorf("paths not tenant-partitioned: %q / %q", pa, pb)
	}
}

func TestConcurrentWritesRaceFree(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	payload := []byte("racing write content goes here, deliberately a bit longer to make hashing nontrivial")

	const N = 32
	var wg sync.WaitGroup
	type result struct {
		sha     string
		deduped bool
		err     error
	}
	results := make(chan result, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := s.NewWriter(KindRaw, testTenant, bk)
			if err != nil {
				results <- result{err: err}
				return
			}
			if _, err := w.Write(payload); err != nil {
				results <- result{err: err}
				return
			}
			sha, _, dedup, err := w.Close()
			results <- result{sha: sha, deduped: dedup, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var firstSHA string
	originals := 0
	for r := range results {
		if r.err != nil {
			t.Errorf("concurrent op: %v", r.err)
			continue
		}
		if firstSHA == "" {
			firstSHA = r.sha
		} else if r.sha != firstSHA {
			t.Errorf("sha drift: %q vs %q", r.sha, firstSHA)
		}
		if !r.deduped {
			originals++
		}
	}
	if originals != 1 {
		t.Errorf("expected exactly 1 non-deduped write, got %d", originals)
	}

	r, err := s.Open(KindRaw, testTenant, bk, firstSHA)
	if err != nil {
		t.Fatalf("Open after concurrent: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, payload) {
		t.Error("post-concurrent payload mismatch")
	}
}

func TestOpenNotFound(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := s.Open(KindRaw, testTenant, bk, zero)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of missing blob: err = %v, want ErrNotFound", err)
	}
	exists, err := s.Exists(KindRaw, testTenant, bk, zero)
	if err != nil {
		t.Errorf("Exists: %v", err)
	}
	if exists {
		t.Error("Exists returned true for missing blob")
	}
}

func TestOpenInvalidSHA(t *testing.T) {
	s := setupStore(t)
	_, err := s.Open(KindRaw, testTenant, fixedBucket(), "not-hex")
	if !errors.Is(err, ErrInvalidSHA) {
		t.Errorf("Open with non-hex sha: err = %v, want ErrInvalidSHA", err)
	}
}

func TestAbort(t *testing.T) {
	s := setupStore(t)
	w, _ := s.NewWriter(KindRaw, testTenant, fixedBucket())
	w.Write([]byte("aborted"))
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	tmpDir := filepath.Join(s.root, string(testTenant), "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty tmp/, got %d entries", len(entries))
	}
}

func TestCloseAfterCloseErrors(t *testing.T) {
	s := setupStore(t)
	w, _ := s.NewWriter(KindRaw, testTenant, fixedBucket())
	w.Write([]byte("x"))
	if _, _, _, err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, _, _, err := w.Close(); err == nil {
		t.Error("second Close should error")
	}
}

func TestWriteAfterCloseErrors(t *testing.T) {
	s := setupStore(t)
	w, _ := s.NewWriter(KindRaw, testTenant, fixedBucket())
	w.Write([]byte("x"))
	w.Close()
	if _, err := w.Write([]byte("y")); err == nil {
		t.Error("Write after Close should error")
	}
}

func TestCheckPermissions(t *testing.T) {
	s := setupStore(t)
	// A fresh store (root only, no tenants) passes.
	if err := s.CheckPermissions(); err != nil {
		t.Errorf("freshly-initialized store should pass: %v", err)
	}

	// Materialize a tenant subtree, then loosen its raw/ dir.
	w, _ := s.NewWriter(KindRaw, testTenant, fixedBucket())
	w.Write([]byte("x"))
	if _, _, _, err := w.Close(); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := s.CheckPermissions(); err != nil {
		t.Errorf("store with a well-permissioned tenant should pass: %v", err)
	}

	rawDir := filepath.Join(s.root, string(testTenant), "raw")
	if err := os.Chmod(rawDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(rawDir, 0o750) })
	if err := s.CheckPermissions(); err == nil {
		t.Error("CheckPermissions should reject a 0755 tenant raw/ dir")
	}
	_ = os.Chmod(rawDir, 0o750)

	// Loosening the root itself is also rejected.
	if err := os.Chmod(s.root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.root, 0o750) })
	if err := s.CheckPermissions(); err == nil {
		t.Error("CheckPermissions should reject a 0755 root")
	}
}

func TestPathShardingShape(t *testing.T) {
	s := setupStore(t)
	bk := BucketFromTime(time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))
	hx := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	p, err := s.PathFor(KindRaw, testTenant, bk, hx)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	want := filepath.Join(s.root, string(testTenant), "raw", "2026", "05", "18", "ab", "cd", hx+".eml")
	if p != want {
		t.Errorf("PathFor sharding wrong: got %q, want %q", p, want)
	}
}

func TestUnknownBucketLayout(t *testing.T) {
	s := setupStore(t)
	bk := BucketUnknown()
	hx := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	p, err := s.PathFor(KindAttachment, testTenant, bk, hx)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	want := filepath.Join(s.root, string(testTenant), "att", "1970", "01", "01", "ab", "cd", hx+".bin")
	if p != want {
		t.Errorf("unknown-bucket path: got %q, want %q", p, want)
	}
}

func TestWalkRecognizesBothBucketShapesAndTenants(t *testing.T) {
	s := setupStore(t)
	day := BucketFromTime(time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))
	unk := BucketUnknown()

	type seed struct {
		tenant  Tenant
		bucket  Bucket
		content string
	}
	for _, c := range []seed{
		{testTenant, day, "alice-in-bucket-payload"},
		{testTenant, unk, "alice-unknown-bucket-payload"},
		{otherTenant, day, "bob-in-bucket-payload"},
	} {
		w, _ := s.NewWriter(KindRaw, c.tenant, c.bucket)
		if _, err := w.Write([]byte(c.content)); err != nil {
			t.Fatalf("write %s/%s: %v", c.tenant, c.bucket, err)
		}
		if _, _, _, err := w.Close(); err != nil {
			t.Fatalf("close %s/%s: %v", c.tenant, c.bucket, err)
		}
	}

	type key struct {
		tenant Tenant
		bucket Bucket
	}
	seen := map[key]int{}
	err := s.Walk(KindRaw, func(_ Kind, tenant Tenant, bucket Bucket, shaHex string, _ string, _ os.FileInfo) error {
		if ValidateSHA(shaHex) != nil {
			t.Errorf("walk returned invalid sha %q", shaHex)
		}
		if tenant.Validate() != nil {
			t.Errorf("walk returned invalid tenant %q", tenant)
		}
		seen[key{tenant, bucket}]++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if seen[key{testTenant, day}] != 1 {
		t.Errorf("expected 1 alice blob in %s bucket, got %d", day, seen[key{testTenant, day}])
	}
	if seen[key{testTenant, unk}] != 1 {
		t.Errorf("expected 1 alice blob in unknown bucket, got %d", seen[key{testTenant, unk}])
	}
	if seen[key{otherTenant, day}] != 1 {
		t.Errorf("expected 1 bob blob in %s bucket, got %d", day, seen[key{otherTenant, day}])
	}
}

// TestWalkSkipsStrayRootEntries confirms a non-tenant file or directory at the
// storage root (e.g. an import checkpoint) is ignored rather than walked.
func TestWalkSkipsStrayRootEntries(t *testing.T) {
	s := setupStore(t)
	bk := fixedBucket()
	w, _ := s.NewWriter(KindRaw, testTenant, bk)
	w.Write([]byte("real blob"))
	if _, _, _, err := w.Close(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A dotfile and an uppercase dir at root must be skipped silently.
	if err := os.WriteFile(filepath.Join(s.root, ".import-checkpoint"), []byte("x"), 0o640); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "NotATenant"), 0o750); err != nil {
		t.Fatalf("mkdir stray: %v", err)
	}
	count := 0
	if err := s.Walk(KindRaw, func(_ Kind, _ Tenant, _ Bucket, _ string, _ string, _ os.FileInfo) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 walked blob, got %d", count)
	}
}
