package blob

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Directory handles make the durability memo safe across unlocked GC pruning:
// an open handle keeps the old inode alive, so os.SameFile detects a path that
// a peer removed and recreated even if the filesystem would otherwise reuse
// its inode number. A cached mtime detects new entries beneath the same inode.
// The bounded LRU avoids turning a large shard tree into an fd leak.
const maxDurableDirHandles = 128

type durableDirEntry struct {
	path    string
	file    *os.File
	modTime time.Time
	elem    *list.Element
}

type durableDirCache struct {
	mu      sync.Mutex
	entries map[string]*durableDirEntry
	lru     list.List
}

// durabilityMarker is legacy bookkeeping retained only so GC can remove
// markers written by older versions. New writes use the in-memory handle
// cache and do not create marker files.
const durabilityMarker = ".durable"

// durableAncestry verifies — and if necessary establishes — that dir's
// directory chain is durable. Called on the fast path, where dir already
// exists.
func (s *Store) durableAncestry(dir string) error {
	return s.syncAncestry(dir)
}

func (s *Store) forgetDurableDirLocked(entry *durableDirEntry) {
	delete(s.durableDirs.entries, entry.path)
	s.durableDirs.lru.Remove(entry.elem)
	_ = entry.file.Close()
}

// durableDirUnchanged reports whether path still names the exact directory
// inode whose entry set was last fsynced by this process.
func (s *Store) durableDirUnchanged(path string) bool {
	c := &s.durableDirs
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[path]
	if entry == nil {
		return false
	}
	openInfo, openErr := entry.file.Stat()
	pathInfo, pathErr := os.Stat(path)
	if openErr != nil || pathErr != nil || !pathInfo.IsDir() ||
		!os.SameFile(openInfo, pathInfo) || !openInfo.ModTime().Equal(entry.modTime) {
		s.forgetDurableDirLocked(entry)
		return false
	}
	c.lru.MoveToFront(entry.elem)
	return true
}

// syncAndRememberDurableDir opens an identity witness before fsync. That
// ordering matters: if GC replaces the path around the fsync, the old handle
// will not match the new path and the replacement is never cached as durable.
func (s *Store) syncAndRememberDurableDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fsyncDir(path)
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() {
		_ = f.Close()
		return fsyncDir(path)
	}
	if err := fsyncDir(path); err != nil {
		_ = f.Close()
		return err
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil
	}
	c := &s.durableDirs
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*durableDirEntry)
	}
	if old := c.entries[path]; old != nil {
		s.forgetDurableDirLocked(old)
	}
	entry := &durableDirEntry{path: path, file: f, modTime: info.ModTime()}
	entry.elem = c.lru.PushFront(entry)
	c.entries[path] = entry
	for len(c.entries) > maxDurableDirHandles {
		oldest := c.lru.Back().Value.(*durableDirEntry)
		s.forgetDurableDirLocked(oldest)
	}
	return nil
}

// syncAncestry fsyncs the unproven suffix from the store root down to dir.
// Every directory before that suffix has an unchanged open-handle witness.
func (s *Store) syncAncestry(dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		if s.durableDirUnchanged(s.root) {
			return nil
		}
		if err := s.syncAndRememberDurableDir(s.root); err != nil {
			return err
		}
		return nil
	}
	paths := []string{s.root}
	cur := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part != "" {
			cur = filepath.Join(cur, part)
			paths = append(paths, cur)
		}
	}
	firstUnproven := 0
	for firstUnproven < len(paths) && s.durableDirUnchanged(paths[firstUnproven]) {
		firstUnproven++
	}
	for _, path := range paths[firstUnproven:] {
		if err := s.syncAndRememberDurableDir(path); err != nil {
			return fmt.Errorf("fsync %s: %w", path, err)
		}
	}
	return nil
}

// markerPathFor returns the legacy durability marker for the date bucket
// containing dir. It is used by compatibility tests and cleanup only.
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

// RemoveDurabilityMarkerIfOnlyEntry deletes a bucket's durability marker when
// it is the only remaining entry, so GC can prune a date bucket whose blobs
// have all been reaped.
//
// It reports whether it removed something. A bucket that still holds blobs, or
// holds anything else at all, is left alone. New versions do not recreate the
// marker after removing it.
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
