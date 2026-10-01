package imapsess

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestDummyPasswordHashIsUsable is the R-059 unit: the fixed dummy hash is a
// valid Argon2id encoding that no password matches — so running VerifyPassword
// against it on the unknown-user/disabled paths equalizes timing without ever
// authenticating anyone.
func TestDummyPasswordHashIsUsable(t *testing.T) {
	if dummyPasswordHash == "" {
		t.Fatal("dummyPasswordHash is empty")
	}
	ok, err := auth.VerifyPassword("any password at all", dummyPasswordHash)
	if err != nil {
		t.Fatalf("dummy hash did not parse as a valid Argon2id hash: %v", err)
	}
	if ok {
		t.Fatal("a password matched the dummy hash; it must never authenticate")
	}
}

// TestLoginUnknownUserPaysKDF is the R-059 integration check: an unknown
// username must pay the Argon2id KDF (tens of ms) rather than returning in
// sub-millisecond time, closing the username-enumeration timing oracle. The
// 5ms floor is far below a real 64 MiB Argon2 cost and far above a bare DB
// miss, so it is robust across machines.
func TestLoginUnknownUserPaysKDF(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A real mailbox with production params so the store is realistic; the test
	// logs in as a DIFFERENT, nonexistent user.
	hash, err := auth.HashPassword("rightpw", auth.DefaultParams())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', $1)`, hash,
	); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	be := &Backend{
		Pool:        db.Pool(),
		BlobStore:   blob.NewStore(t.TempDir()),
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout: 10 * time.Second,
	}
	sess := be.NewSession()
	sess.remoteIP = "1.2.3.4"

	start := time.Now()
	if err := sess.Login("does-not-exist", "whatever"); err == nil {
		t.Fatal("Login for a nonexistent user succeeded")
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("unknown-user Login returned in %v; expected the equalizing KDF cost (R-059)", elapsed)
	}
}
