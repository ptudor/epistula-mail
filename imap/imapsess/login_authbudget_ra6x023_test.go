package imapsess

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// A canceled session cannot start database or KDF work or become authenticated.
// Every account category gets the same unavailable result before lookup. Budget
// contention itself is exercised by the concurrent single-slot test below.
func TestCanceledLoginDoesNotAuthenticate(t *testing.T) {
	be, _ := authBudgetFixture(t, NewAuthBudget(64*1024))

	cases := []struct {
		name     string
		username string
		password string
		wantText string
	}{
		{"nonexistent account keeps the generic failure", "nobody", "whatever", "backend unavailable"},
		{"disabled account keeps the generic failure", "disabled", "whatever", "backend unavailable"},
		{"real account with a wrong password", "alice", "wrongpw", "backend unavailable"},
		{"real account with the right password", "alice", "rightpw", "backend unavailable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := be.NewSession()
			defer func() { _ = sess.Close() }()

			// The client is gone before the verification can start.
			sess.sessCancel()

			err := sess.Login(tc.username, tc.password)
			if err == nil {
				t.Fatal("a login on a closed session must not succeed")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected an error mentioning %q, got %v", tc.wantText, err)
			}
			if sess.mailboxID != 0 {
				t.Fatal("a refused login must not authenticate the session")
			}
		})
	}
}

// TestLoginSucceedsUnderASingleSlotBudget pins that rationing does not break
// ordinary logins: with a budget that admits exactly one verification at a
// time, concurrent logins from many distinct source addresses all still get a
// correct answer — they simply queue.
//
// This is the "many distinct IPs" case: per-IP limiters never
// see a threshold crossed, so the memory budget is the only thing bounding the
// work.
func TestLoginSucceedsUnderASingleSlotBudget(t *testing.T) {
	be, _ := authBudgetFixture(t, NewAuthBudget(64*1024))

	const clients = 12
	var wg sync.WaitGroup
	results := make([]error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinct source address per client, exactly as a distributed
			// attacker (or a large IPv6 allocation) would present.
			sess := be.newSession("2001:db8::" + string(rune('a'+i%26)))
			defer func() { _ = sess.Close() }()
			results[i] = sess.Login("alice", "rightpw")
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("client %d: a queued login must still succeed: %v", i, err)
		}
	}
}

// TestLoginRejectsAnOversizedStoredHash pins that a corrupted or imported PHC
// row asking for more work than the verifier accepts is refused before it can
// occupy a rationed slot — it must not be able to monopolize the budget.
func TestLoginRejectsAnOversizedStoredHash(t *testing.T) {
	be, db := authBudgetFixture(t, NewAuthBudget(64*1024))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	good, err := auth.HashPassword("rightpw", auth.DefaultParams())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	oversized := strings.Replace(good, "m=65536", "m=8388608", 1) // 8 GiB
	if oversized == good {
		t.Fatal("fixture did not alter the memory parameter")
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET password_hash = $1 WHERE name = 'alice'`, oversized,
	); err != nil {
		t.Fatalf("update hash: %v", err)
	}

	sess := be.NewSession()
	defer func() { _ = sess.Close() }()

	done := make(chan error, 1)
	go func() { done <- sess.Login("alice", "rightpw") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an out-of-range stored hash must not authenticate")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an out-of-range stored hash was allowed to start hashing")
	}
	if sess.mailboxID != 0 {
		t.Fatal("session authenticated against an out-of-range hash")
	}
}

// authBudgetFixture returns a Backend wired to the given budget, with an
// enabled account `alice` (password "rightpw") and a disabled account
// `disabled`.
func authBudgetFixture(t *testing.T, budget *AuthBudget) (*Backend, *storage.DB) {
	t.Helper()
	db, _ := pgtest.Open(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hash, err := auth.HashPassword("rightpw", auth.DefaultParams())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', $1)`, hash,
	); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash, disabled_at) VALUES ('disabled', $1, now())`, hash,
	); err != nil {
		t.Fatalf("insert disabled mailbox: %v", err)
	}

	be := &Backend{
		Pool:        db.Pool(),
		BlobStore:   blob.NewStore(t.TempDir()),
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout: 10 * time.Second,
		AuthBudget:  budget,
	}
	return be, db
}
