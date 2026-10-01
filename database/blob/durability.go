package blob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Every writer syncs its directory ancestry, including a chain created by a
// peer. A date-bucket marker cannot certify new shards or a pruned/recreated
// bucket, and a process cache cannot detect either. Markers from older builds
// remain harmless bookkeeping, never evidence permitting a skipped fsync.

// durabilityMarker is legacy bookkeeping retained for compatibility with
// existing stores and GC. The leading dot keeps it out of the way of shard
// directories, and Walk already ignores anything that is not a
// `<64 hex>.<ext>` blob.
const durabilityMarker = ".durable"

// durableAncestry verifies — and if necessary establishes — that dir's
// directory chain is durable. Called on the fast path, where dir already
// exists.
func (s *Store) durableAncestry(dir string) error {
	if err := s.syncAncestry(dir); err != nil {
		return err
	}
	return s.markAncestryDurable(dir)
}

// markAncestryDurable creates and syncs the marker for dir's bucket, after the
// caller has synced the chain.
func (s *Store) markAncestryDurable(dir string) error {
	marker, ok := s.markerPathFor(dir)
	if !ok {
		return nil
	}
	if _, cached := s.durableDirs.Load(marker); cached {
		return nil
	}

	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, s.fileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			s.durableDirs.Store(marker, struct{}{})
			return nil
		}
		// A peer daemon may own the bucket in shared-group mode and the mode
		// bits may not permit us to create here. That is not a reason to fail
		// a delivery: fall back to syncing the chain on every write from this
		// process, which is correct, just slower.
		return nil
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("fsync durability marker %s: %w", marker, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close durability marker %s: %w", marker, closeErr)
	}
	// Persist the bookkeeping entry. Writers still sync their whole chain
	// independently of this marker on every write.
	if err := fsyncDir(filepath.Dir(marker)); err != nil {
		return fmt.Errorf("fsync %s after marking durable: %w", filepath.Dir(marker), err)
	}
	s.durableDirs.Store(marker, struct{}{})
	return nil
}

// syncAncestry fsyncs every directory from the store root down to dir. Used
// when a chain exists but nothing proves it is durable.
func (s *Store) syncAncestry(dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fsyncDir(s.root)
	}
	cur := s.root
	if err := fsyncDir(filepath.Dir(cur)); err != nil {
		return err
	}
	if err := fsyncDir(cur); err != nil {
		return fmt.Errorf("fsync %s: %w", cur, err)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if err := fsyncDir(cur); err != nil {
			return fmt.Errorf("fsync %s: %w", cur, err)
		}
	}
	return nil
}

// markerPathFor returns the durability marker for the date bucket containing
// dir. dir is a shard directory (`<tenant>/<kind>/yyyy/mm/dd/aa/bb`), so the
// bucket is two levels up. Returns ok=false for a path that is not inside a
// bucket, which the caller treats as "nothing to prove here".
func markerPathFor(root, dir string) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// tenant/kind/yyyy/mm/dd/aa/bb
	if len(parts) != 7 {
		return "", false
	}
	bucket := filepath.Join(root, filepath.Join(parts[:5]...))
	return filepath.Join(bucket, durabilityMarker), true
}

func (s *Store) markerPathFor(dir string) (string, bool) {
	return markerPathFor(s.root, dir)
}

// RemoveDurabilityMarkerIfOnlyEntry deletes a bucket's durability marker when
// it is the only remaining entry, so GC can prune a date bucket whose blobs
// have all been reaped.
//
// It reports whether it removed something. A bucket that still holds blobs, or
// holds anything else at all, is left alone — the marker is only disposable
// once there is nothing left for it to protect. The next write into the bucket
// recreates it, syncing the chain again, which is the protocol working as
// intended rather than an exception to it.
func RemoveDurabilityMarkerIfOnlyEntry(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	if len(entries) != 1 || entries[0].Name() != durabilityMarker {
		return false
	}
	return os.Remove(filepath.Join(dir, durabilityMarker)) == nil
}
