// Package blob is the content-addressed blob store backing
// database. Blobs are sha256-named files on disk, isolated
// per owning mailbox (the tenant), then sharded by date (yyyy/mm/dd) and then
// by the first two bytes of the hash to keep directory sizes bounded and to
// make per-tenant and per-period rsync / backup / ZFS-dataset policies
// trivial. The on-disk shape is:
//
//	${root}/<tenant>/raw/<yyyy/mm/dd>/aa/bb/<sha>.eml
//	${root}/<tenant>/att/<yyyy/mm/dd>/aa/bb/<sha>.bin
//	${root}/<tenant>/tmp/blob-*                        (atomic-write staging)
//
// Writes are atomic and race-safe within a (tenant, bucket): concurrent
// writers of the same content into the same tenant+bucket both succeed and
// the loser is reported as deduplicated.
//
// Dedup runs per (tenant, bucket) only. The same bytes delivered to two
// different mailboxes produce two on-disk blobs — that is the whole point of
// tenant isolation (a user's corpus is a single subtree that can be backed
// up, restored, deleted, or encrypted independently). Two identical bytes
// arriving for one mailbox on different days also produce two blobs — an
// intentional simplification (per-day buckets give clean rotation, and most
// modern mail carries per-recipient UUID tracking URLs that defeat cross-day
// dedup anyway).
//
// The per-tenant tmp/ directory is deliberately a sibling of raw/ and att/
// *inside* the tenant subtree, not a single shared ${root}/tmp: the finalize
// step is os.Link (no cross-device rename fallback), so the staging file must
// live on the same filesystem as its final path. When each ${root}/<tenant>
// is its own ZFS dataset, a shared tmp would make every link cross-device
// (EXDEV).
//
// Blobs whose date can't be derived bucket under 1970/01/01 — a real,
// parseable date that predates email and is therefore an unambiguous
// "unknown" sentinel without a magic-string special case in the path
// parser.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kind selects which subtree a blob lives in.
type Kind string

const (
	// KindRaw is the original RFC 5322 message bytes.
	KindRaw Kind = "raw"
	// KindAttachment is a MIME part body extracted at ingest.
	KindAttachment Kind = "att"
)

var (
	// ErrInvalidSHA is returned for any hex string that is not exactly
	// 64 lower-case hexadecimal characters.
	ErrInvalidSHA = errors.New("blob: invalid sha256 hex")
	// ErrInvalidBucket is returned for any bucket value that is not
	// "yyyy/mm/dd" with a valid calendar date.
	ErrInvalidBucket = errors.New("blob: invalid bucket")
	// ErrInvalidTenant is returned for any tenant (mailbox-name) value that
	// is not a single path-safe component (see tenantRe).
	ErrInvalidTenant = errors.New("blob: invalid tenant")
	// ErrNotFound is returned by Open when no blob exists at the given hash.
	ErrNotFound = errors.New("blob: not found")
)

var (
	sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	bucketRe    = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2}$`)
	// tenantRe bounds a tenant to one lower-case, path-safe directory
	// component. The leading [a-z0-9] anchor rejects "", ".", "..", and any
	// dotfile, so the value can never escape its parent via path traversal;
	// the class excludes '/' so it is always exactly one component. This is
	// the canonical mailbox-name charset — kept in lockstep with the
	// mailboxes.name CHECK constraint and the admin mailbox-add validator.
	tenantRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ValidateSHA returns ErrInvalidSHA if s is not a canonical sha256 hex string
// (exactly 64 lower-case hex characters). Validating up front lets us
// construct disk paths from the hash without any path-traversal risk.
func ValidateSHA(s string) error {
	if !sha256HexRe.MatchString(s) {
		return ErrInvalidSHA
	}
	return nil
}

// Tenant is the owning-mailbox directory component a blob lives under. It is
// always the canonical mailboxes.name value, validated to a single path-safe
// component so it can be used to construct a disk path without any
// path-traversal risk. The zero value is invalid; construct via ParseTenant.
type Tenant string

// ParseTenant validates s and returns it unchanged as a Tenant. It is a pure
// validator — it does NOT lower-case or otherwise mutate the value — because
// the same string is used by the writer (path), the GC walker (read back from
// the directory name), the advisory-lock key, and the mailbox-id resolution;
// any transform here would desync those callers. Names are expected to already
// be canonical (lower-cased at mailbox creation).
func ParseTenant(s string) (Tenant, error) {
	if !tenantRe.MatchString(s) {
		return "", ErrInvalidTenant
	}
	return Tenant(s), nil
}

// Validate returns ErrInvalidTenant if t is not a well-formed tenant.
func (t Tenant) Validate() error {
	if !tenantRe.MatchString(string(t)) {
		return ErrInvalidTenant
	}
	return nil
}

// String returns the tenant as its on-disk path component.
func (t Tenant) String() string { return string(t) }

// Bucket is the "yyyy/mm/dd" date partition a blob lives under. The zero
// value is invalid; construct via BucketFromTime, BucketUnknown, or
// ParseBucket.
type Bucket string

// BucketUnknown returns the sentinel date (1970/01/01, the Unix epoch and
// safely earlier than email existed) for blobs whose real date can't be
// derived. Using a real parseable date instead of a literal like "unknown"
// keeps the path parser uniform.
func BucketUnknown() Bucket { return "1970/01/01" }

// BucketFromTime returns the bucket for t, in UTC. The zero time yields
// BucketUnknown.
func BucketFromTime(t time.Time) Bucket {
	if t.IsZero() {
		return BucketUnknown()
	}
	return Bucket(t.UTC().Format("2006/01/02"))
}

// ParseBucket validates s and returns it as a Bucket. Accepts "yyyy/mm/dd"
// with a real calendar date.
func ParseBucket(s string) (Bucket, error) {
	if !bucketRe.MatchString(s) {
		return "", ErrInvalidBucket
	}
	if _, err := time.Parse("2006/01/02", s); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidBucket, err)
	}
	return Bucket(s), nil
}

// Validate returns an error if b is not a well-formed bucket. Callers that
// construct Buckets via BucketFromTime / BucketUnknown can skip this; it
// is mostly useful for values that came from on-disk paths.
func (b Bucket) Validate() error {
	if b == "" {
		return ErrInvalidBucket
	}
	_, err := ParseBucket(string(b))
	return err
}

// String returns the bucket as its on-disk path component.
func (b Bucket) String() string { return string(b) }

// Store is a rooted content-addressed blob store.
type Store struct {
	root string
	// dirMode/fileMode govern how blob dirs and files are created. Default is
	// owner-only writes (0750 / 0640). A shared-group deployment (see
	// SetGroupWritable) uses setgid group-writable modes so a cooperating
	// daemon in the storage group — epistula-imap doing APPEND — can write
	// into the tree the LDA created.
	dirMode  os.FileMode
	fileMode os.FileMode

	// onUnknownSubtree, when set, is notified of every storage_root child
	// whose name is not a valid tenant. See SetOnUnknownSubtree.
	onUnknownSubtree func(name string)

	// onMisplacedBlob, when set, is notified of every file that looks like a
	// blob but sits under shard directories that do not match its digest
	// (RA6X-062). See SetOnMisplacedBlob.
	onMisplacedBlob func(kind Kind, tenant Tenant, sha256Hex, path string)

	// verifyMode selects how thoroughly an existing blob is checked before it
	// is reused (RA6X-035). Zero value is VerifyHash.
	verifyMode VerifyMode

	// logger records integrity events. Nil is silent.
	logger *slog.Logger

	// durableDirs memoizes date buckets this process has proven durable, so
	// the ancestry check costs one stat per bucket rather than per blob
	// (RA6X-036).
	durableDirs sync.Map
}

// NewStore returns a Store rooted at root. The directory tree is not created
// until Init is called. Defaults to owner-only-write modes (dirs 0750, files
// 0640); call SetGroupWritable(true) for a shared-group deployment.
func NewStore(root string) *Store {
	return &Store{root: root, dirMode: 0o750, fileMode: 0o640}
}

// SetGroupWritable switches the store between owner-only and shared-group
// permission modes. In shared-group mode dirs are created 2770 (setgid, so
// blobs a cooperating daemon writes inherit the storage group) and files 0660,
// which is what lets epistula-imap (a member of the epistula-database storage
// group) satisfy an IMAP APPEND — CreateTemp in <tenant>/tmp and os.Link into
// <tenant>/raw both need directory write permission. Owner-only mode keeps the
// original 0750/0640. CheckPermissions rejects only "other" bits, so both
// modes pass. Must be called before the first write. See R-007.
func (s *Store) SetGroupWritable(shared bool) {
	if shared {
		// os.ModeSetgid (not the raw 0o2000 bit, which Go's syscallMode drops)
		// so children inherit the storage group; 0770 dirs / 0660 files give
		// the group write. ensureDir chmods past a restrictive umask.
		s.dirMode, s.fileMode = os.ModeSetgid|0o770, 0o660
	} else {
		s.dirMode, s.fileMode = 0o750, 0o640
	}
}

// chmodShared applies s.dirMode to a directory level this process just
// created, forcing the setgid + group-write bits past a restrictive umask
// (MkdirAll applies its mode through the umask, which would otherwise strip
// group-write and defeat cross-daemon APPEND). chmod(2) requires euid ==
// owner, so if a cooperating daemon won the creation race and owns the
// level, the chmod fails EPERM — tolerated as long as the directory already
// carries the shared bits (the winner's own chmod walk sets them).
func (s *Store) chmodShared(dir string) error {
	err := osChmod(dir, s.dirMode)
	if err == nil {
		return nil
	}
	if fi, serr := os.Stat(dir); serr == nil && fi.IsDir() &&
		fi.Mode()&os.ModeSetgid != 0 && fi.Mode().Perm()&0o070 == 0o070 {
		return nil
	}
	return fmt.Errorf("chmod %s: %w", dir, err)
}

// ensureDir creates dir (and any missing ancestors under the store root) with
// s.dirMode. In shared-group mode it then chmods each level it just created —
// and only those — so the setgid + group-write bits survive a restrictive
// process umask. Pre-existing levels are left alone: their creator already
// set the shared bits (this daemon's earlier walk, the peer daemon's, or the
// operator's chmod -R g+ws), and chmod(2) requires ownership, so chmodding a
// level the peer daemon owns would fail EPERM and break every cross-daemon
// APPEND/delivery into an existing tree (R-007). A fast path skips the walk
// when the leaf already exists.
func (s *Store) ensureDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		// Present — but present is not the same as DURABLE (RA6X-036).
		//
		// This returned immediately, so one writer could observe directories
		// another writer had created but not yet fsynced, sync only the leaf,
		// and commit its database row. If the creator then exited before
		// syncing its ancestors, a power loss could erase the whole directory
		// chain containing the acknowledged blob — a row with no blob, the one
		// state the design forbids. A process-local mutex cannot help: the LDA
		// and the IMAP server are separate processes.
		//
		// durableAncestry syncs every ancestor, even if a marker exists.
		return s.durableAncestry(dir)
	}

	rel, relErr := filepath.Rel(s.root, dir)
	if relErr != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
		// dir is the root itself or outside it; create + (shared mode) chmod.
		if err := os.MkdirAll(dir, s.dirMode); err != nil {
			return err
		}
		if s.dirMode&os.ModeSetgid != 0 {
			return s.chmodShared(dir)
		}
		return nil
	}

	parts := make([]string, 0, 8)
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if p != "" {
			parts = append(parts, p)
		}
	}

	// Find the deepest already-existing ancestor: its directory entry set
	// changes when we create the first new level below it, so it (and every
	// newly-created level except the leaf) must be fsynced for the new entries
	// to survive a crash (R-028).
	deepestExisting := s.root
	firstNew := 0
	probe := s.root
	for i, part := range parts {
		probe = filepath.Join(probe, part)
		if fi, err := os.Stat(probe); err == nil && fi.IsDir() {
			deepestExisting = probe
			firstNew = i + 1
			continue
		}
		break
	}

	if err := os.MkdirAll(dir, s.dirMode); err != nil {
		return err
	}

	// Shared-group mode: chmod only the levels MkdirAll just created (which
	// this process owns). Walking pre-existing levels would EPERM on dirs the
	// peer daemon owns (R-007).
	if s.dirMode&os.ModeSetgid != 0 {
		cur := deepestExisting
		for i := firstNew; i < len(parts); i++ {
			cur = filepath.Join(cur, parts[i])
			if err := s.chmodShared(cur); err != nil {
				return err
			}
		}
	}

	// Existing ancestors may have been created by a peer that has not synced
	// them yet. Sync the whole chain on both paths, without chmodding peers.
	return s.durableAncestry(dir)
}

// Root returns the storage root directory.
func (s *Store) Root() string { return s.root }

// Init ensures the storage root exists, idempotently. Per-tenant subtrees
// (<tenant>/raw, <tenant>/att, <tenant>/tmp) and their date/shard subdirs are
// created on demand at write time, since the set of tenants is dynamic.
func (s *Store) Init() error {
	if err := os.MkdirAll(s.root, s.dirMode); err != nil {
		return fmt.Errorf("create %s: %w", s.root, err)
	}
	// In shared-group mode force the mode past umask (MkdirAll masks it).
	// chmodShared tolerates an operator-created root owned by another user
	// as long as it already carries the shared bits.
	if s.dirMode&os.ModeSetgid != 0 {
		if err := s.chmodShared(s.root); err != nil {
			return err
		}
	}
	// MkdirAll can create multiple levels above the configured root. Persist
	// those entries too; syncing only the root cannot persist its own name.
	for dir := s.root; ; dir = filepath.Dir(dir) {
		if err := fsyncDir(dir); err != nil {
			return fmt.Errorf("fsync root ancestry %s: %w", dir, err)
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return nil
}

// CheckPermissions fails if the storage root — or any existing per-tenant
// subdir — has any "other" permission bits set, or is not a directory.
// Production daemons should refuse to start when this fails. Per-tenant
// raw/att/tmp dirs are created on demand with 0750; this walks the tenants
// that already exist and verifies the same, but does not require any tenant
// to be present (a fresh deployment has none).
func (s *Store) CheckPermissions() error {
	if err := checkDirPerm(s.root); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read %s: %w", s.root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // stray files (e.g. import checkpoint) are not our concern
		}
		if _, terr := ParseTenant(e.Name()); terr != nil {
			s.noteUnknownSubtree(e.Name())
			continue // not a tenant subtree
		}
		tenantRoot := filepath.Join(s.root, e.Name())
		if err := checkDirPerm(tenantRoot); err != nil {
			return err
		}
		for _, sub := range []string{"raw", "att", "tmp"} {
			p := filepath.Join(tenantRoot, sub)
			if _, statErr := os.Stat(p); errors.Is(statErr, os.ErrNotExist) {
				continue // created on first write of that kind
			}
			if err := checkDirPerm(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDirPerm verifies p is a directory with no "other" permission bits.
func checkDirPerm(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	if mode := info.Mode().Perm(); mode&0o007 != 0 {
		return fmt.Errorf("%s is world-accessible (mode %#o); chmod to 0750", p, mode)
	}
	return nil
}

// PathFor returns the canonical disk path for a content-addressed blob owned
// by tenant. The hash and tenant MUST validate; an invalid value returns the
// corresponding error and the construction of the disk path never proceeds —
// there is no path-traversal surface here.
func (s *Store) PathFor(kind Kind, tenant Tenant, bucket Bucket, sha256Hex string) (string, error) {
	if err := ValidateSHA(sha256Hex); err != nil {
		return "", err
	}
	if err := tenant.Validate(); err != nil {
		return "", err
	}
	if err := bucket.Validate(); err != nil {
		return "", err
	}
	sub, ext, err := kindSubExt(kind)
	if err != nil {
		return "", err
	}
	return filepath.Join(
		s.root, string(tenant), sub, string(bucket),
		sha256Hex[0:2], sha256Hex[2:4],
		sha256Hex+ext,
	), nil
}

// tenantTmpDir is the per-tenant staging directory, a sibling of raw/ and att/
// so os.Link stays within one filesystem (see package doc / EXDEV note).
func (s *Store) tenantTmpDir(tenant Tenant) string {
	return filepath.Join(s.root, string(tenant), "tmp")
}

// SweepStaleTmp removes staging files (tmp/blob-*) older than grace from every
// tenant's tmp/ directory and returns how many it unlinked. These orphans are
// left behind when the LDA is killed by a signal or exits via the per-delivery
// watchdog (os.Exit) between CreateTemp and the finalizing link/Abort — nothing
// else ever reaps them, so 50 MiB staging files would accumulate forever inside
// every tenant subtree (R-031). The grace (same as blob marking) protects an
// in-flight write's tmp file: a file younger than the cutoff may belong to a
// delivery still running. Best-effort — a per-file unlink race with a
// concurrent writer (ENOENT) is not an error.
func (s *Store) SweepStaleTmp(grace time.Duration) (int, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", s.root, err)
	}
	cutoff := time.Now().Add(-grace)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tenant, terr := ParseTenant(e.Name())
		if terr != nil {
			s.noteUnknownSubtree(e.Name())
			continue // not a tenant subtree
		}
		tmpDir := s.tenantTmpDir(tenant)
		files, rerr := os.ReadDir(tmpDir)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue // no staging dir yet for this tenant
			}
			return removed, fmt.Errorf("read tmp %s: %w", tmpDir, rerr)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasPrefix(f.Name(), "blob-") {
				continue
			}
			info, ierr := f.Info()
			if ierr != nil {
				if errors.Is(ierr, os.ErrNotExist) {
					continue
				}
				return removed, fmt.Errorf("stat staging file: %w", ierr)
			}
			if info.ModTime().After(cutoff) {
				continue // young — may be an in-flight write's staging file
			}
			if rmErr := os.Remove(filepath.Join(tmpDir, f.Name())); rmErr != nil && !os.IsNotExist(rmErr) {
				return removed, fmt.Errorf("unlink stale tmp %s: %w", f.Name(), rmErr)
			}
			removed++
		}
	}
	return removed, nil
}

// Exists reports whether a blob is present on disk under the given tenant and
// bucket.
func (s *Store) Exists(kind Kind, tenant Tenant, bucket Bucket, sha256Hex string) (bool, error) {
	p, err := s.PathFor(kind, tenant, bucket, sha256Hex)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Open returns a reader for the blob in the given tenant and bucket.
// ErrNotFound is returned when the blob is absent. The caller MUST Close the
// returned reader.
func (s *Store) Open(kind Kind, tenant Tenant, bucket Bucket, sha256Hex string) (io.ReadCloser, error) {
	p, err := s.PathFor(kind, tenant, bucket, sha256Hex)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Writer streams content into a temporary file while accumulating sha256.
// Close finalizes by linking the tmp file into its content-addressed
// location under the writer's bucket; concurrent writers of the same bytes
// into the same bucket safely deduplicate.
type Writer struct {
	store   *Store
	kind    Kind
	tenant  Tenant
	bucket  Bucket
	f       *os.File
	tmpPath string
	h       hash.Hash
	written int64
	closed  bool
}

// NewWriter returns a Writer for the given kind, tenant, and bucket. The
// tenant's tmp/ directory is created on demand so the staging file is a
// sibling of the eventual final path (keeping os.Link intra-filesystem).
func (s *Store) NewWriter(kind Kind, tenant Tenant, bucket Bucket) (*Writer, error) {
	if _, _, err := kindSubExt(kind); err != nil {
		return nil, err
	}
	if err := tenant.Validate(); err != nil {
		return nil, err
	}
	if err := bucket.Validate(); err != nil {
		return nil, err
	}
	tmpDir := s.tenantTmpDir(tenant)
	if err := s.ensureDir(tmpDir); err != nil {
		return nil, fmt.Errorf("create tenant tmp: %w", err)
	}
	f, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return nil, fmt.Errorf("create tmp: %w", err)
	}
	if err := f.Chmod(s.fileMode); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("chmod tmp: %w", err)
	}
	return &Writer{
		store:   s,
		kind:    kind,
		tenant:  tenant,
		bucket:  bucket,
		f:       f,
		tmpPath: f.Name(),
		h:       sha256.New(),
	}, nil
}

// Write streams bytes into the tmp file and updates the sha256 hash.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("blob: writer is closed")
	}
	n, err := w.f.Write(p)
	if n > 0 {
		_, _ = w.h.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

// Abort discards the in-progress tmp file. Safe to call after a partial Write.
// Returns the os.Remove error if any.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.f.Close()
	return os.Remove(w.tmpPath)
}

// Close finalizes the blob. Returns:
//   - sha256Hex: the content hash (only meaningful on success)
//   - size: total bytes written
//   - deduped: true if the blob was already on disk under the same bucket;
//     the tmp file was discarded and no new bytes were committed
//   - err: any failure
//
// Durability ordering: fsync(tmp), link(tmp, final), unlink(tmp),
// fsync(parent dir of final). A power loss before the link is harmless
// (tmp is garbage); after the link the blob is durable.
func (w *Writer) Close() (sha256Hex string, size int64, deduped bool, err error) {
	if w.closed {
		err = errors.New("blob: writer already closed")
		return
	}
	w.closed = true

	if syncErr := w.f.Sync(); syncErr != nil {
		_ = w.f.Close()
		_ = os.Remove(w.tmpPath)
		err = fmt.Errorf("fsync tmp: %w", syncErr)
		return
	}
	if closeErr := w.f.Close(); closeErr != nil {
		_ = os.Remove(w.tmpPath)
		err = fmt.Errorf("close tmp: %w", closeErr)
		return
	}

	sha256Hex = hex.EncodeToString(w.h.Sum(nil))
	size = w.written

	finalPath, pErr := w.store.PathFor(w.kind, w.tenant, w.bucket, sha256Hex)
	if pErr != nil {
		_ = os.Remove(w.tmpPath)
		err = pErr
		return
	}

	parent := filepath.Dir(finalPath)
	if mkErr := w.store.ensureDir(parent); mkErr != nil {
		_ = os.Remove(w.tmpPath)
		err = fmt.Errorf("mkdir parent: %w", mkErr)
		return
	}

	// link is atomic and fails with EEXIST when the destination already
	// exists, which is exactly the dedup case (per-bucket). It can also fail
	// ENOENT if a concurrent gc pruneEmptyDirs rmdir'd the freshly-created
	// parent between the ensureDir above and here (the prune runs unlocked
	// against other shas in the same shard dir). Re-create the parent and retry
	// once, closing that window instead of bouncing the delivery EX_TEMPFAIL
	// (R-046).
	linkErr := osLink(w.tmpPath, finalPath)
	if linkErr != nil && errors.Is(linkErr, os.ErrNotExist) {
		if mkErr := w.store.ensureDir(parent); mkErr != nil {
			_ = os.Remove(w.tmpPath)
			err = fmt.Errorf("recreate parent after prune race: %w", mkErr)
			return
		}
		linkErr = osLink(w.tmpPath, finalPath)
	}
	if linkErr != nil {
		if errors.Is(linkErr, os.ErrExist) {
			// Dedup hit — but "a file exists at the content-addressed path" is
			// not the same fact as "that file holds this content" (RA6X-035).
			// A truncated or altered file at a valid digest path was accepted
			// as authoritative, and the correct bytes we had just written were
			// deleted: the delivery acknowledged a message pointing at known-
			// bad content, and the last copy of the good bytes went with the
			// temporary file.
			//
			// This is the one moment repair is free: the correct bytes are in
			// hand, verified by the hash we just computed over them.
			repaired, vErr := w.store.verifyOrRepair(finalPath, w.tmpPath, sha256Hex, size)
			if vErr != nil {
				_ = os.Remove(w.tmpPath)
				err = vErr
				return
			}
			deduped = !repaired
			if repaired {
				w.store.logf("blob: repaired a corrupt existing blob from the bytes being stored",
					"path", finalPath, "sha16", shortSHA(sha256Hex))
			}
		} else {
			_ = os.Remove(w.tmpPath)
			err = fmt.Errorf("link tmp to final: %w", linkErr)
			return
		}
	}
	_ = os.Remove(w.tmpPath)

	// Always fsync the parent dir, including the dedup path (R-028). On an
	// EEXIST/dedup, the pre-existing entry may have been created by a process
	// that crashed before its own fsyncDir — committing a PG row against a dir
	// entry a power loss can still erase would leave a row with no blob, the
	// state the design forbids. The fsync is cheap.
	if dirErr := fsyncDir(parent); dirErr != nil {
		err = fmt.Errorf("fsync parent dir: %w", dirErr)
		return
	}
	return
}

// EnsureContent guarantees the blob for data exists on disk under the given
// bucket, rewriting it from the in-memory bytes if it is missing. shaHex must
// be the sha256 of data; a mismatch after rewrite is reported as an error
// (internal bug, never expected). Returns rewritten=true when the blob had to
// be recreated.
//
// This is the ingest side of the GC race protocol: a delivery whose blob
// write deduplicated against an existing file may lose that file to a
// concurrent `gc sweep` before its database transaction commits. Callers
// invoke EnsureContent from inside the ingest transaction, after the
// transaction has taken the blob's advisory lock, so the existence check
// here cannot race the sweeper's unlink.
func (s *Store) EnsureContent(kind Kind, tenant Tenant, bucket Bucket, shaHex string, data []byte) (rewritten bool, err error) {
	exists, err := s.Exists(kind, tenant, bucket, shaHex)
	if err != nil {
		return false, err
	}
	if exists {
		// Present is not the same as correct (RA6X-035). This returned
		// immediately on existence and never compared length or hash, so a
		// truncated or altered file made the transaction commit a row
		// pointing at content the store could not serve.
		//
		// The caller holds the correct bytes and this runs under the blob's
		// advisory lock, so a mismatch is repairable here and only here.
		path, pErr := s.PathFor(kind, tenant, bucket, shaHex)
		if pErr != nil {
			return false, pErr
		}
		ok, vErr := s.blobMatches(path, shaHex, int64(len(data)))
		if vErr != nil {
			return false, vErr
		}
		if ok {
			return false, nil
		}
		s.logf("blob: existing blob does not match its content address; rewriting from the message being stored",
			"path", path, "sha16", shortSHA(shaHex))
		// Fall through to the rewrite below, which replaces it atomically.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return false, fmt.Errorf("remove corrupt blob %s: %w", path, rmErr)
		}
	}
	w, err := s.NewWriter(kind, tenant, bucket)
	if err != nil {
		return false, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Abort()
		return false, err
	}
	gotSHA, _, _, err := w.Close()
	if err != nil {
		return false, err
	}
	if gotSHA != shaHex {
		return false, fmt.Errorf("blob: EnsureContent sha mismatch: have %s, want %s", gotSHA, shaHex)
	}
	return true, nil
}

// WalkFunc receives one blob per visit. tenant is the owning-mailbox subtree
// and bucket is the on-disk bucket path component the blob was found under.
type WalkFunc func(kind Kind, tenant Tenant, bucket Bucket, sha256Hex string, path string, info os.FileInfo) error

// SetOnUnknownSubtree sets fn to be called once per storage_root child whose
// name is not a valid tenant, during Walk / CheckPermissions / SweepStaleTmp.
//
// Those three all skip such a directory — right for stray files, but it means
// a subtree left behind by a mailbox whose name was legal under an older rule,
// or one an operator created by hand, or one restored with the wrong case
// ("JDoe/"), is invisible to gc mark forever: never marked, never swept, never
// permission-checked, never reported. The store silently grows. Reporting them
// is all this does — an unrecognized directory could be an operator's staging
// area, so nothing here deletes anything (RO5X-034).
func (s *Store) SetOnUnknownSubtree(fn func(name string)) {
	s.onUnknownSubtree = fn
}

// noteUnknownSubtree reports a non-tenant directory, if a callback is set.
func (s *Store) noteUnknownSubtree(name string) {
	if s.onUnknownSubtree != nil {
		s.onUnknownSubtree(name)
	}
}

// Walk visits every blob of the given kind on disk, across all tenants. It
// enumerates the per-tenant subtrees under root (skipping non-directories and
// any entry that is not a valid tenant name, e.g. a stray import checkpoint
// file), then walks each `<tenant>/<kind>` tree. Entries whose path shape does
// not match the expected `{yyyy/mm/dd}/aa/bb/<sha>.ext` layout are silently
// skipped (stray files are not the walker's responsibility). Returned errors
// from visit halt the walk.
func (s *Store) Walk(kind Kind, visit WalkFunc) error {
	sub, ext, err := kindSubExt(kind)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tenant, terr := ParseTenant(e.Name())
		if terr != nil {
			s.noteUnknownSubtree(e.Name())
			continue // not a tenant subtree (dotfile, uppercase, etc.)
		}
		if err := s.walkTenant(kind, tenant, sub, ext, visit); err != nil {
			return err
		}
	}
	return nil
}

// walkTenant walks one tenant's `<tenant>/<sub>` subtree.
func (s *Store) walkTenant(kind Kind, tenant Tenant, sub, ext string, visit WalkFunc) error {
	base := filepath.Join(s.root, string(tenant), sub)
	return filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(info.Name()) != ext {
			return nil
		}
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return nil
		}
		bucket, shaHex, ok := splitWalkPath(rel, ext)
		if !ok {
			// A file that looks like a blob but sits under the wrong shard is
			// reported, not silently skipped: GC would otherwise operate on a
			// path it reconstructs and never on the one the file is at, so the
			// misplaced copy survives forever while every pass claims to have
			// handled it (RA6X-062). Deleting or moving it is an operator
			// decision — a restore or a manual migration put it there.
			if sha, misplaced := splitWalkPathMisplaced(rel, ext); misplaced && s.onMisplacedBlob != nil {
				s.onMisplacedBlob(kind, tenant, sha, path)
			}
			return nil
		}
		if ValidateSHA(shaHex) != nil {
			return nil
		}
		if bucket.Validate() != nil {
			return nil
		}
		return visit(kind, tenant, bucket, shaHex, path, info)
	})
}

// splitWalkPath parses a path relative to <root>/<kind> into (bucket, sha).
// The on-disk shape is always yyyy/mm/dd/aa/bb/<sha>.ext — the
// unknown-date bucket uses 1970/01/01, so there is no special case.
// Returns ok=false for stray files / partial paths.
func splitWalkPath(rel, ext string) (Bucket, string, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 6 {
		return "", "", false
	}
	file := parts[5]
	if !strings.HasSuffix(file, ext) {
		return "", "", false
	}
	shaHex := strings.TrimSuffix(file, ext)

	// The SHARD components must agree with the digest (RA6X-062).
	//
	// This skipped parts[3] and parts[4] entirely, so a file at
	// `yyyy/mm/dd/00/11/abcd….eml` was reported as a canonical blob for that
	// digest — while PathFor reconstructs `yyyy/mm/dd/ab/cd/abcd….eml`. GC
	// then marked and swept against a path the file is not at: the misplaced
	// copy survives every pass while mark reports work against another
	// location, and two such files for one digest produce duplicate keys in a
	// single candidate upsert batch.
	//
	// A misplaced file is not reported as a blob and is NOT deleted — it is
	// surfaced to the caller as a misplaced entry, because a restore or a
	// manual migration that put it there is an operator's to resolve, not
	// GC's to guess at.
	if len(shaHex) < 4 || parts[3] != shaHex[0:2] || parts[4] != shaHex[2:4] {
		return "", "", false
	}
	return Bucket(parts[0] + "/" + parts[1] + "/" + parts[2]), shaHex, true
}

// splitWalkPathMisplaced reports whether rel looks like a blob whose shard
// directories do not match its digest — the case splitWalkPath now rejects.
//
// Kept separate so the walker can tell "not a blob at all" (a stray file,
// which is nobody's problem) from "a blob in the wrong place" (which an
// operator needs to know about), rather than silently skipping both
// (RA6X-062).
func splitWalkPathMisplaced(rel, ext string) (shaHex string, misplaced bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 6 {
		return "", false
	}
	file := parts[5]
	if !strings.HasSuffix(file, ext) {
		return "", false
	}
	sha := strings.TrimSuffix(file, ext)
	if ValidateSHA(sha) != nil {
		return "", false
	}
	if parts[3] == sha[0:2] && parts[4] == sha[2:4] {
		return "", false
	}
	return sha, true
}

func kindSubExt(k Kind) (string, string, error) {
	switch k {
	case KindRaw:
		return "raw", ".eml", nil
	case KindAttachment:
		return "att", ".bin", nil
	default:
		return "", "", fmt.Errorf("blob: unknown kind %q", k)
	}
}

// osLink is os.Link behind a package var so tests can inject the ENOENT that a
// gc prune-race produces and exercise the Writer.Close retry (R-046).
var osLink = os.Link

// osChmod is os.Chmod behind a package var so tests can observe which levels
// the shared-mode chmod walk touches and inject the EPERM a foreign-owned
// level produces (R-007).
var osChmod = os.Chmod

// fsyncDir opens a directory and calls Sync, persisting the most recent
// directory-entry change. It is a package var so tests can observe the fsync
// calls the durability protocol makes (R-028).
var fsyncDir = func(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SetOnMisplacedBlob registers a callback for files that look like blobs but
// sit under shard directories that disagree with their digest (RA6X-062).
//
// Walk skips such files — they are not canonical blobs and must not be treated
// as references or as reap candidates — but an operator needs to know they
// exist, because GC will otherwise report work against a reconstructed path
// while the real file survives untouched. The callback is a diagnostic, never
// an instruction to delete: a restore or a manual migration put the file
// there, and resolving it is the operator's call.
func (s *Store) SetOnMisplacedBlob(fn func(kind Kind, tenant Tenant, sha256Hex, path string)) {
	s.onMisplacedBlob = fn
}
