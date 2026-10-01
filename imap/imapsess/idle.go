package imapsess

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5/pgxpool"
)

// idleHeartbeat is the default WaitForNotification timeout before pinging
// the connection, used when Backend.IdleHeartbeat is unset. RFC 9051
// mandates that IMAP servers re-poll the client at least every 30 minutes;
// 29 minutes leaves a comfortable margin.
const idleHeartbeat = 29 * time.Minute

// idlePool returns the dedicated IDLE LISTEN pool when configured
// (idle_conn_pool_size), falling back to the main query pool. The
// dedicated pool keeps many simultaneous IDLE sessions from starving
// Pool.MaxConns.
func (b *Backend) idlePool() *pgxpool.Pool {
	if b.IdlePool != nil {
		return b.IdlePool
	}
	return b.Pool
}

// heartbeat returns the configured IDLE re-poll interval (idle_timeout).
func (b *Backend) heartbeat() time.Duration {
	if b.IdleHeartbeat > 0 {
		return b.IdleHeartbeat
	}
	return idleHeartbeat
}

// idleConsume owns a dedicated pgx connection (from the idle pool when
// configured, else the main pool) for the duration of the IDLE — a regular
// query connection can't double as a LISTEN consumer because pgx may
// multiplex statements across the pool.
func (s *Session) idleConsume(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	// IDLE is only meaningful with a selected folder — without one
	// there's no "this folder" to subscribe to. Per RFC 9051 the
	// server may IDLE in authenticated state too, but until SELECT
	// happens there's nothing to notify on, so we just wait for
	// stop.
	if s.selectedFolderID == 0 {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}

	// ONE cancellable context, created before anything can block (RA6X-022).
	//
	// The stop channel used to be converted to cancellation only after the
	// pool acquisition and the LISTEN, and the acquisition ran on a background
	// context. With the dedicated IDLE pool full — twenty simultaneous
	// listeners across all accounts is the default, so this is ordinary, not
	// exotic — a second IDLE waited on Acquire forever: DONE could not reach
	// it, closing the connection could not reach it, and neither could SIGTERM.
	// The count query it ran afterwards was unbounded for the same reason.
	//
	// Deriving it here means every one of those blocking points is cancelled
	// by DONE, by session close and by daemon shutdown, and the connection is
	// released on the way out whichever fires.
	idleCtx, idleCancel := context.WithCancel(ctx)
	defer idleCancel()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-stop:
			idleCancel()
		case <-stopWatch:
		case <-idleCtx.Done():
		}
	}()

	err := s.idleListen(idleCtx, w)
	if idleCtx.Err() != nil {
		return nil
	}
	if err != nil {
		s.be.Logger.Warn("IDLE listener unavailable; polling", "err", err)
	}
	// A lost listener is an optimization failure. Poll with a finite interval
	// until DONE; database recovery resumes updates without a client reconnect.
	interval := s.be.heartbeat()
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if idleCtx.Err() != nil {
			return nil
		}
		if err := s.reconcile(idleCtx, w, true); err != nil {
			switch {
			case idleCtx.Err() != nil:
				// DONE, a closed connection or shutdown cancelled the poll
				// while it was in the database, and the query failed with
				// that cancellation. IDLE ended as asked; returning the
				// error answered the client's DONE with NO (OPS-007). The
				// listener path above already reads it this way.
				return nil
			case errors.Is(err, errReconcileBackend):
				// The next poll repeats whatever this one could not report.
				// Returning would end the polling while the client still
				// waits in IDLE, so it would receive nothing until its own
				// DONE, which clients send as rarely as every 29 minutes
				// (OPS-007).
				s.be.Logger.Warn("IDLE poll failed; retrying", "err", err)
			default:
				return err
			}
		}
		select {
		case <-idleCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Session) idleListen(ctx context.Context, w *imapserver.UpdateWriter) error {
	setupCtx, cancel := context.WithTimeout(ctx, s.setupTimeout())
	conn, err := s.be.idlePool().Acquire(setupCtx)
	if err != nil {
		cancel()
		return err
	}
	defer conn.Release()
	_, err = conn.Exec(setupCtx, `LISTEN mail_arrived`)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, `UNLISTEN mail_arrived`)
	}()
	if err := s.reconcile(ctx, w, true); err != nil {
		return err
	}
	want := strconv.FormatInt(s.selectedFolderID, 10)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait, cancel := context.WithTimeout(ctx, s.be.heartbeat())
		n, err := conn.Conn().WaitForNotification(wait)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil || (n != nil && n.Channel == "mail_arrived" && n.Payload == want) {
			if err := s.reconcile(ctx, w, true); err != nil {
				return err
			}
		}
	}
}

// countMessages reports a folder's message count.
//
// IDLE no longer uses it to answer a notification — a bare recount claims a
// count the client's sequence numbers may not support, so IDLE reconciles the
// session view instead (RA6X-001). It is kept because the LISTEN/NOTIFY
// integration test uses it as its oracle, and because a plain count is still
// the clearest way to assert "the notification arrived and the row is there".
func (s *Session) countMessages(ctx context.Context, folderID int64) (uint32, error) {
	var n int64
	if err := s.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1`, folderID,
	).Scan(&n); err != nil {
		return 0, err
	}
	return uint32(n), nil
}

// setupTimeout bounds IDLE's connection acquisition and LISTEN. It reuses the
// per-command statement timeout, so an operator who tunes query patience tunes
// this with it; the fallback matches queryCtx's.
func (s *Session) setupTimeout() time.Duration {
	if t := s.be.StmtTimeout; t > 0 {
		return t
	}
	return 10 * time.Second
}
