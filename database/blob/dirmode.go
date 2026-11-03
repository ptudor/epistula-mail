package blob

import (
	"os"
	"syscall"
)

// SharedGroupUmask is the process umask for a shared-group deployment
// (SetGroupWritable(true)): group bits pass through, "other" bits never do.
//
// With it, mkdir(2) creates a blob directory with the shared bits already set.
// Without it the bits depend on a chmod that follows the mkdir, and a process
// killed between the two leaves the level at its umask-masked mode. Postfix runs
// the delivery agent with umask 077, so that level is 2700, which the peer
// daemon's group cannot enter. On junia on 2026-11-03 the delivery wall clock
// killed the delivery agent at that point during a disk stall. Every later
// FETCH of the message the retry stored beneath the level failed with
// "permission denied".
const SharedGroupUmask = 0o007

// UseSharedGroupUmask sets the process umask to SharedGroupUmask and returns
// the previous value. A shared-group daemon calls it once at startup, before
// it creates files: the umask is per process, not per goroutine.
//
// Every file these daemons create outside the blob store asks for an explicit
// mode with no "other" bits (0600, 0640, 0700, 0750), so this umask leaves those
// modes unchanged.
func UseSharedGroupUmask() int {
	return syscall.Umask(SharedGroupUmask)
}

// currentEUID is os.Geteuid behind a package var so tests can stand in for a
// level owned by the peer daemon without running as root.
var currentEUID = os.Geteuid

// requiredDirBits is the part of the store's directory mode that the peer
// daemon depends on: setgid and group rwx in shared-group mode, and group r-x
// in owner-only mode, where the IMAP server reads through the group.
func (s *Store) requiredDirBits() os.FileMode {
	return s.dirMode & (os.ModeSetgid | 0o070)
}

// repairDirMode restores the store's directory mode on a level that lacks the
// bits the peer daemon depends on. The writer that created such a level died
// between mkdir and chmod. Everything that later finds the level existing
// assumes, by design, that its creator already set the bits (ensureDirSlowMode,
// R-007), so without this the level stays unreadable indefinitely.
//
// Only a level this process owns is changed. chmod(2) requires ownership, and a
// level owned by the peer daemon is left for that daemon to repair the next
// time it passes. The failure is logged rather than returned: the caller's own
// write does not depend on the peer's access, and refusing it would turn one
// unreadable directory into deferred mail.
//
// chmod is the change to make, os.Chmod for a path or (*os.File).Chmod for a
// directory the caller holds open. info is the level's current FileInfo.
func (s *Store) repairDirMode(path string, info os.FileInfo, chmod func(os.FileMode) error) {
	need := s.requiredDirBits()
	if info.Mode()&need == need {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if int(st.Uid) != currentEUID() {
		s.logf("blob directory lacks the store's group bits and belongs to another user; that user's daemon repairs it on its next write there",
			"path", path, "mode", info.Mode().String(), "owner_uid", st.Uid)
		return
	}
	if err := chmod(s.dirMode); err != nil {
		s.logf("blob directory lacks the store's group bits and could not be repaired",
			"path", path, "mode", info.Mode().String(), "err", err)
		return
	}
	if s.logger != nil {
		s.logger.Warn("repaired blob directory mode left by an interrupted writer",
			"path", path, "from", info.Mode().String(), "to", (os.ModeDir | s.dirMode).String())
	}
}
