package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Bounded local-file reads for the maintenance commands (RA6X-034).
//
// Every recovery entry point — the Maildir importer, the blob importer, the
// import verifier and reparse — called os.ReadFile, which allocates the whole
// file before the parser can enforce MaxMessageBytes. A corrupt, unexpectedly
// large or actively growing source could exhaust memory during the very pass
// meant to recover from a problem, and a FIFO or device node under the source
// tree would block it indefinitely. The delivery stream's size protection did
// not reach any of them.
//
// A pre-read stat is not the answer on its own: the file may grow between the
// stat and the read. The bound has to be on the read itself.

// ErrFileTooLarge reports a local file that exceeds the configured maximum.
// Callers classify it as a per-item failure, not a pass-level abort: one
// oversized file in an archive must not stop the recovery of the rest.
var ErrFileTooLarge = errors.New("file exceeds the configured maximum message size")

// ErrNotRegularFile reports a source entry that is not an ordinary file. A
// FIFO would block the read forever and a device node would produce nonsense;
// both are refused deliberately rather than hung on.
//
// Symlinks are followed, because documented import workflows rely on them
// (a Maildir assembled from symlinks into an archive) — os.Open follows the
// link and the mode check then applies to its target.
var ErrNotRegularFile = errors.New("source is not a regular file")

// readBounded reads at most max bytes from path, refusing anything larger and
// anything that is not a regular file.
//
// It reads max+1 bytes so "exactly at the limit" is distinguishable from
// "over it" — the same trick the LDA uses on stdin — rather than trusting a
// stat the file can invalidate.
func readBounded(path string, max int64) ([]byte, error) {
	max = boundOrDefault(max)
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrFileTooLarge, path, max)
	}
	return data, nil
}

// hashBounded streams a file through h-style accumulation without buffering
// it, for integrity checks that need the digest and not the bytes. It applies
// the same regular-file policy and the same maximum, so an oversized file is
// reported rather than read.
//
// Returns the number of bytes read.
func hashBounded(path string, max int64, w io.Writer) (int64, error) {
	max = boundOrDefault(max)
	f, err := openRegular(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(w, io.LimitReader(f, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("%w: %s exceeds %d bytes", ErrFileTooLarge, path, max)
	}
	return n, nil
}

// openRegular opens path for reading, refusing anything that is not an
// ordinary file.
//
// The type check happens BEFORE the open, deliberately: opening a FIFO for
// reading blocks until a writer appears, so checking the mode of an already
// open file is too late — the import has already hung. Symlinks are followed,
// because documented import workflows assemble a Maildir from links into an
// archive; the check then applies to the link's target.
//
// O_NONBLOCK closes the residual race where the path is replaced between the
// stat and the open: on a FIFO it makes the open return immediately, and it is
// a no-op for a regular file. The post-open stat catches the substitution.
//
// This is a TYPE check only. The SIZE bound stays on the read, because a stat
// is a claim the file can invalidate by growing.
func openRegular(path string) (*os.File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s (%s)", ErrNotRegularFile, path, fi.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s (%s)", ErrNotRegularFile, path, fi.Mode().Type())
	}
	return f, nil
}

// boundOrDefault turns an unset limit into the default maximum rather than into
// an unbounded read.
//
// A caller that has not threaded its configuration through is a bug, but the
// safe failure for that bug is "bounded by the documented default", not "read
// whatever is on disk" — which is the behaviour RA6X-034 is about. The default
// matches Config.Limits.MaxMessageBytes.
func boundOrDefault(max int64) int64 {
	if max > 0 {
		return max
	}
	return defaultMaxMessageBytes
}

// defaultMaxMessageBytes mirrors DefaultConfig().Limits.MaxMessageBytes.
const defaultMaxMessageBytes int64 = 52_428_800
