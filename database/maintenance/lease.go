// Package maintenance enforces an offline store-wide rename barrier. All
// processes using blobs hold a shared root-directory lock for their lifetime;
// administrative maintenance transitions require its exclusive lock. The
// durable database flag bridges the operator's filesystem move between commands.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrOffline = errors.New("mail store is in offline maintenance")

// Acquire takes no extra database connection and creates no lockfile. Keep
// the returned lease until every request/session, blob read and write ends.
// Read-only scans may allow an absent root because they cannot create a tenant.
func Acquire(ctx context.Context, root string, pool *pgxpool.Pool, exclusive, allowMissing bool) (*Lease, error) {
	f, err := os.Open(root)
	if err != nil && !(allowMissing && !exclusive && errors.Is(err, os.ErrNotExist)) {
		return nil, fmt.Errorf("open storage root for maintenance barrier: %w", err)
	}
	lease := &Lease{file: f}
	if f != nil {
		info, err := f.Stat()
		if err != nil || !info.IsDir() {
			lease.Close()
			return nil, fmt.Errorf("storage root must be a directory")
		}
		mode := syscall.LOCK_SH | syscall.LOCK_NB
		if exclusive {
			mode = syscall.LOCK_EX | syscall.LOCK_NB
		}
		if err := syscall.Flock(int(f.Fd()), mode); err != nil {
			lease.Close()
			return nil, fmt.Errorf("%w: stop IMAP/API, Postfix and all blob/import/reparse/GC processes and wait for them to exit: %v", ErrOffline, err)
		}
	}
	if !exclusive {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var active bool
		if err := pool.QueryRow(qctx, `SELECT EXISTS(SELECT 1 FROM mailboxes WHERE maintenance_at IS NOT NULL)`).Scan(&active); err != nil {
			lease.Close()
			return nil, fmt.Errorf("check offline maintenance: %w", err)
		}
		if active {
			lease.Close()
			return nil, ErrOffline
		}
	}
	return lease, nil
}

type Lease struct{ file *os.File }

func (l *Lease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

var processLeases struct {
	sync.Mutex
	leases []*Lease
}

// Daemon shutdown may leave a force-closed request unwinding after runServe
// returns. Retain the file descriptor until actual process exit, which kills
// every session too; neither a deferred Close nor os.File's finalizer may open
// a maintenance window before then. Command-scoped callers use Close instead.
func (l *Lease) HoldUntilExit() {
	processLeases.Lock()
	defer processLeases.Unlock()
	processLeases.leases = append(processLeases.leases, l)
}
