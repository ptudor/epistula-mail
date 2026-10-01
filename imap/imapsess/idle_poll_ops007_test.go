package imapsess

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pollGate intercepts one query of the IDLE poll, chosen by its SQL, so a test
// can place DONE or a database failure at an exact point inside reconcile
// instead of hoping a timer lands there (OPS-007).
type pollGate struct {
	match func(sql string) bool
	// action returns the context the intercepted query runs with.
	action func(ctx context.Context) context.Context
	armed  atomic.Bool
	hit    chan struct{}
}

func newPollGate(match func(string) bool, action func(context.Context) context.Context) *pollGate {
	return &pollGate{match: match, action: action, hit: make(chan struct{})}
}

func (g *pollGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !g.match(d.SQL) || !g.armed.CompareAndSwap(true, false) {
		return ctx
	}
	close(g.hit)
	return g.action(ctx)
}

func (g *pollGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// waitHit waits for the armed query to start.
func (g *pollGate) waitHit(t *testing.T) {
	t.Helper()
	select {
	case <-g.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the IDLE poll never ran the gated query")
	}
}

func isCommit(sql string) bool { return strings.EqualFold(strings.TrimSpace(sql), "commit") }

func isFlagRead(sql string) bool {
	return strings.HasPrefix(sql, "SELECT uid, flags, mod_seq FROM messages")
}

// untilDone holds the query until its context ends: DONE cancels the IDLE,
// and with it the poll's query, while the poll is in the database.
func untilDone(ctx context.Context) context.Context {
	<-ctx.Done()
	return ctx
}

// expired runs the query with a context already past its deadline, as when
// the statement timeout expires, so the query fails while IDLE goes on.
func expired(ctx context.Context) context.Context {
	c, cancel := context.WithDeadline(ctx, time.Now())
	// The deadline has passed, so the context already carries
	// DeadlineExceeded; cancel only releases its resources.
	cancel()
	return c
}

// idleWire is one IMAP connection to a server whose IDLE always polls.
type idleWire struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (c *idleWire) send(line string) {
	c.t.Helper()
	if _, err := fmt.Fprint(c.conn, line+"\r\n"); err != nil {
		c.t.Fatal(err)
	}
}

// until reads lines until one satisfies ok and returns it.
func (c *idleWire) until(ok func(string) bool) string {
	c.t.Helper()
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatal(err)
		}
		if ok(line) {
			return line
		}
	}
}

// tagged reads up to the tagged response for tag and returns it.
func (c *idleWire) tagged(tag string) string {
	c.t.Helper()
	return c.until(func(l string) bool { return strings.HasPrefix(l, tag+" ") })
}

// idlePollWire serves the mutation fixture over IMAP with the LISTEN pool
// closed, so every IDLE falls back to polling at once, and with gate tracing
// the main pool. It returns the fixture, an untraced pool for the test's own
// writes, and a connection that has SELECTed INBOX.
func idlePollWire(t *testing.T, gate *pollGate) (*Session, *pgxpool.Pool, *idleWire) {
	t.Helper()
	f, listenerPool := idlePoolFixture(t, 1)
	// A closed pool refuses Acquire immediately. The listener is unavailable
	// either way; this way the poll starts without waiting out the setup
	// timeout, which stays long so that no deadline but the gate's can end a
	// query.
	listenerPool.Close()
	f.be.IdleHeartbeat = 20 * time.Millisecond

	plain := f.be.Pool
	cfg := plain.Config()
	cfg.ConnConfig.Tracer = gate
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	f.be.Pool = traced

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := f.be.NewSession()
			s.mailboxID, s.mailboxName, s.tenant = f.mailboxID, f.mailboxName, f.tenant
			return s, &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(ln)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	w := &idleWire{t: t, conn: conn, r: bufio.NewReader(conn)}
	w.until(func(string) bool { return true }) // greeting
	w.send("a SELECT INBOX")
	if got := w.tagged("a"); !strings.HasPrefix(got, "a OK") {
		t.Fatalf("SELECT: %q", got)
	}
	return f, plain, w
}

// changeFlags commits a flag change on UID 1 the way STORE does: the folder's
// highest_modseq moves, so a poll cannot take the unchanged-folder path.
func changeFlags(t *testing.T, pool *pgxpool.Pool, folderID int64, flag string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE folders SET highest_modseq=highest_modseq+1 WHERE id=$1`, folderID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE messages SET flags=ARRAY[$2::text],mod_seq=mod_seq+1 WHERE folder_id=$1 AND uid=1`, folderID, flag); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestIdlePollDoneDuringReconcileAnswersOK is the OPS-007 root cause of the
// intermittent TestRemainingIdlePollsWithExhaustedListenerPool failure.
//
// DONE cancels the IDLE's context. When it arrived while the polling fallback
// was inside reconcile, the query failed with "context canceled" and the poll
// loop returned that error as IDLE's result, so the server answered DONE with
// NO instead of OK. The listener path already treated a cancelled context as
// the end of IDLE; the polling loop did not. The gate holds the poll's commit
// until DONE has cancelled it, which is the window the timed test hit about
// half the time.
func TestIdlePollDoneDuringReconcileAnswersOK(t *testing.T) {
	gate := newPollGate(isCommit, untilDone)
	_, _, w := idlePollWire(t, gate)

	gate.armed.Store(true)
	w.send("b IDLE")
	if got := w.until(func(string) bool { return true }); !strings.HasPrefix(got, "+") {
		t.Fatalf("IDLE: %q", got)
	}
	gate.waitHit(t)
	w.send("DONE")
	if got := w.tagged("b"); !strings.HasPrefix(got, "b OK") {
		t.Fatalf("DONE during a poll was answered %q, want OK", got)
	}
	// The session is still usable.
	w.send("c NOOP")
	if got := w.tagged("c"); !strings.HasPrefix(got, "c OK") {
		t.Fatalf("NOOP after IDLE: %q", got)
	}
}

// TestIdlePollRetriesAfterDatabaseFailure is the second OPS-007 defect. A poll
// whose commit or flag read failed returned the error and ended the polling,
// but the client was still in IDLE: it received nothing more until its own
// DONE, which was then answered NO. The poll now retries such a failure, and
// the next poll reports what the failed one could not.
func TestIdlePollRetriesAfterDatabaseFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match func(string) bool
		// changeFirst commits the flag change before the failure, so the
		// failure interrupts a poll that is reporting it.
		changeFirst bool
	}{
		{name: "commit of an unchanged folder", match: isCommit},
		{name: "flag read of a changed folder", match: isFlagRead, changeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newPollGate(tc.match, expired)
			f, plain, w := idlePollWire(t, gate)

			w.send("b IDLE")
			if got := w.until(func(string) bool { return true }); !strings.HasPrefix(got, "+") {
				t.Fatalf("IDLE: %q", got)
			}
			gate.armed.Store(true)
			if tc.changeFirst {
				changeFlags(t, plain, f.selectedFolderID, "$ops007")
				gate.waitHit(t)
			} else {
				gate.waitHit(t)
				changeFlags(t, plain, f.selectedFolderID, "$ops007")
			}
			got := w.until(func(l string) bool {
				return strings.Contains(l, "$ops007") || strings.HasPrefix(l, "b ")
			})
			if !strings.Contains(got, "FETCH") {
				t.Fatalf("after a failed poll the client got %q, want the flag update", got)
			}
			w.send("DONE")
			if got := w.tagged("b"); !strings.HasPrefix(got, "b OK") {
				t.Fatalf("DONE: %q", got)
			}
		})
	}
}
