package imapsess

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// throttleFixture builds a session backed by a real PG + LoginLimiter
// so Login() exercises the throttle end-to-end.
func throttleFixture(t *testing.T, attempts int, window, cooldown time.Duration) (*Backend, string) {
	t.Helper()
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use fast Argon2 params so the test doesn't burn seconds per
	// attempt — the throttle behaviour is what we're checking, not
	// the KDF cost.
	hash, err := auth.HashPassword("rightpw", auth.Params{
		Memory: 1024, Iterations: 1, Parallel: 1, KeyLen: 16, SaltLen: 8,
	})
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', $1)`,
		hash,
	); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	be := &Backend{
		Pool:         pool,
		BlobStore:    blob.NewStore(t.TempDir()),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout:  10 * time.Second,
		LoginLimiter: NewLoginThrottle(attempts, window, cooldown),
		PerMailbox:   NewMailboxSessionLimiter(0), // no per-mailbox cap for this test
	}
	return be, "1.2.3.4"
}

func TestLoginThrottleEndToEnd(t *testing.T) {
	be, ip := throttleFixture(t, 3, 60*time.Second, 30*time.Second)

	// 3 wrong-password attempts: all return authFailed, throttle records.
	for i := 0; i < 3; i++ {
		sess := be.NewSessionForConn(&fakeAddr{ip: ip})
		err := sess.Login("alice", "wrongpw")
		if err == nil {
			t.Fatalf("attempt %d: wrong password should fail", i+1)
		}
	}
	// 4th attempt with the RIGHT password — must fail because the IP
	// is locked out and we short-circuit before the verify.
	sess := be.NewSessionForConn(&fakeAddr{ip: ip})
	if err := sess.Login("alice", "rightpw"); err == nil {
		t.Fatal("right password from locked-out IP must still fail")
	}
	if sess.mailboxID != 0 {
		t.Errorf("mailboxID = %d, want 0 (login refused)", sess.mailboxID)
	}

	// Different IP, right password — succeeds.
	other := be.NewSessionForConn(&fakeAddr{ip: "9.9.9.9"})
	if err := other.Login("alice", "rightpw"); err != nil {
		t.Errorf("different IP should not be throttled: %v", err)
	}
	other.Close()
}

func TestLoginThrottleSuccessRecorded(t *testing.T) {
	be, ip := throttleFixture(t, 3, 60*time.Second, 30*time.Second)

	// 2 wrong, then 1 right → success clears the counter.
	for i := 0; i < 2; i++ {
		sess := be.NewSessionForConn(&fakeAddr{ip: ip})
		_ = sess.Login("alice", "wrongpw")
	}
	sess := be.NewSessionForConn(&fakeAddr{ip: ip})
	if err := sess.Login("alice", "rightpw"); err != nil {
		t.Fatalf("right password should succeed: %v", err)
	}
	sess.Close()

	// Now 2 more wrong — should NOT lock out, since the success cleared.
	for i := 0; i < 2; i++ {
		bad := be.NewSessionForConn(&fakeAddr{ip: ip})
		_ = bad.Login("alice", "wrongpw")
	}
	good := be.NewSessionForConn(&fakeAddr{ip: ip})
	if err := good.Login("alice", "rightpw"); err != nil {
		t.Errorf("after success-clear, 2 more failures should not lock: %v", err)
	}
}

func TestPerMailboxSessionCap(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hash, _ := auth.HashPassword("pw", auth.Params{
		Memory: 1024, Iterations: 1, Parallel: 1, KeyLen: 16, SaltLen: 8,
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('bob', $1)`, hash,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	be := &Backend{
		Pool:        pool,
		BlobStore:   blob.NewStore(t.TempDir()),
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout: 10 * time.Second,
		PerMailbox:  NewMailboxSessionLimiter(2),
	}

	s1 := be.NewSessionForConn(&fakeAddr{ip: "1.1.1.1"})
	s2 := be.NewSessionForConn(&fakeAddr{ip: "2.2.2.2"})
	s3 := be.NewSessionForConn(&fakeAddr{ip: "3.3.3.3"})

	if err := s1.Login("bob", "pw"); err != nil {
		t.Fatalf("1st: %v", err)
	}
	if err := s2.Login("bob", "pw"); err != nil {
		t.Fatalf("2nd: %v", err)
	}
	err := s3.Login("bob", "pw")
	if err == nil {
		t.Fatal("3rd login should be refused by per-mailbox cap")
	}
	imapErr, ok := err.(*imap.Error)
	if !ok || imapErr.Code != imap.ResponseCodeLimit {
		t.Errorf("3rd: err = %v, want LIMIT", err)
	}

	// Closing s1 frees the slot.
	s1.Close()
	s4 := be.NewSessionForConn(&fakeAddr{ip: "4.4.4.4"})
	if err := s4.Login("bob", "pw"); err != nil {
		t.Errorf("after Close, slot should reopen: %v", err)
	}
	s2.Close()
	s4.Close()
}

type fakeAddr struct{ ip string }

func (a *fakeAddr) Network() string { return "tcp" }
func (a *fakeAddr) String() string  { return a.ip + ":12345" }
