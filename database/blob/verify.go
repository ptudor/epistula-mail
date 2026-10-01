package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Integrity verification at the reuse boundary (RA6X-035).
//
// A content-addressed path is a claim, not a guarantee. The store treated
// "a file exists at this digest's path" as "that file holds this content", so
// a truncated or altered blob — a half-written file from a crash, a bad
// restore, a filesystem repair — was accepted as authoritative forever. The
// consequences run in both directions:
//
//   - a NEW delivery of the same message, which has the correct bytes in hand,
//     saw EEXIST, set deduped=true, deleted its own correct temporary file and
//     acknowledged the message. The last good copy was destroyed by the code
//     that could have restored it.
//   - EnsureContent, which exists precisely to re-verify a blob survived a
//     concurrent sweep, checked only existence — so the transaction committed
//     a row pointing at content the store could not serve.
//
// Verification happens where repair is possible: both callers hold the correct
// bytes, so a mismatch is fixed rather than merely reported. Nothing is ever
// written from unverified input — the replacement is the caller's own data,
// whose hash the caller computed.

// VerifyMode selects how much of an existing blob is checked before it is
// reused.
type VerifyMode int

const (
	// VerifyHash reads the existing file and compares its sha256 to the
	// content address. This is the only mode that detects equal-size
	// corruption — a flipped bit, a partially overwritten file, a restore that
	// put the wrong message at the right path — and it is the default, because
	// the failure it prevents is silently serving the wrong mail.
	//
	// The cost is one read of a file the store would otherwise have trusted,
	// and it is paid only on a dedup hit or a post-sweep re-verify, not on
	// every write.
	VerifyHash VerifyMode = iota

	// VerifySize compares only the file's length. It catches truncation, which
	// is the common corruption, at the cost of a stat instead of a read. An
	// operator with very large attachments and a verified-integrity filesystem
	// underneath (e.g. ZFS with checksums)
	// may reasonably choose this.
	VerifySize

	// VerifyOff restores the pre-RA6X-035 behaviour: existence is enough.
	// Provided so the change can be backed out operationally without a
	// rebuild; it is not a supported configuration.
	VerifyOff
)

// SetVerifyMode selects how existing blobs are checked before reuse.
func (s *Store) SetVerifyMode(m VerifyMode) { s.verifyMode = m }

// SetLogger attaches a logger for integrity events. Without one the store is
// silent, which is what the pure-unit tests want.
func (s *Store) SetLogger(l *slog.Logger) { s.logger = l }

func (s *Store) logf(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Error(msg, args...)
	}
}

// blobMatches reports whether the file at path really holds the content its
// path claims. wantSize is the expected length; a negative value means "do not
// check the length", used where the caller does not know it.
//
// A missing file is not a match and not an error: the caller's next step is to
// write it.
func (s *Store) blobMatches(path, wantSHA string, wantSize int64) (bool, error) {
	if s.verifyMode == VerifyOff {
		return true, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("blob path %s is not a regular file", path)
	}
	if wantSize >= 0 && fi.Size() != wantSize {
		return false, nil
	}
	if s.verifyMode == VerifySize {
		return true, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA, nil
}

// verifyOrRepair is the EEXIST branch of a blob write: the destination already
// exists and the caller's verified bytes are still at tmpPath.
//
// Returns repaired=true when the existing file was wrong and has been replaced
// with the caller's bytes. Returns false with no error when the existing file
// is correct, which is the ordinary dedup hit.
//
// The replacement is atomic from a reader's point of view: the caller's file is
// linked into place under a temporary name in the same directory and renamed
// over the bad one, so no reader ever sees a partially written blob, and a
// crash mid-repair leaves either the old file or the new one.
func (s *Store) verifyOrRepair(finalPath, tmpPath, wantSHA string, wantSize int64) (repaired bool, err error) {
	ok, err := s.blobMatches(finalPath, wantSHA, wantSize)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil
	}

	dir := filepath.Dir(finalPath)
	// Writers can reach EEXIST before acquiring the database blob lock.
	// A shared .repair name lets them unlink each other's staged content.
	f, err := os.CreateTemp(dir, ".repair-*")
	if err != nil {
		return false, err
	}
	staging := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(staging)
		return false, err
	}
	if err := os.Remove(staging); err != nil {
		return false, err
	}
	defer os.Remove(staging)
	if err := osLink(tmpPath, staging); err != nil {
		return false, fmt.Errorf("stage repair for %s: %w", finalPath, err)
	}
	if err := os.Rename(staging, finalPath); err != nil {
		_ = os.Remove(staging)
		return false, fmt.Errorf("replace corrupt blob %s: %w", finalPath, err)
	}
	if err := fsyncDir(dir); err != nil {
		return false, fmt.Errorf("fsync %s after repair: %w", dir, err)
	}
	return true, nil
}

// shortSHA is the log-safe 16-character prefix of a digest.
func shortSHA(sha string) string {
	if len(sha) >= 16 {
		return sha[:16]
	}
	return sha
}

// ContentMatches reports whether the blob at the given coordinates exists and
// holds the content its address claims, per the store's verify mode.
//
// Exported for callers that must decide whether an existing record can be
// trusted without having the bytes in hand — the LDA's duplicate-delivery
// acknowledgement, and maintenance tools that would otherwise derive new
// metadata from unverified raw bytes (RA6X-035).
//
// wantSize may be negative to skip the length check.
func (s *Store) ContentMatches(kind Kind, tenant Tenant, bucket Bucket, shaHex string, wantSize int64) (bool, error) {
	path, err := s.PathFor(kind, tenant, bucket, shaHex)
	if err != nil {
		return false, err
	}
	return s.blobMatches(path, shaHex, wantSize)
}
