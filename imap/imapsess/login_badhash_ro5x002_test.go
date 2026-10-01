package imapsess

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestLoginAgainstMalformedStoredHash is the RO5X-002 end-to-end check.
//
// mailboxes.password_hash is a plain TEXT column with no format CHECK. A row
// carrying t=0 used to reach argon2.IDKey, which panics rather than
// erroring — taking down the client's connection mid-LOGIN. The bounds check
// in auth.VerifyPassword now turns it into an ordinary auth failure.
func TestLoginAgainstMalformedStoredHash(t *testing.T) {
	// Each case is a hash a corrupt row / foreign Argon2 binding could
	// plausibly produce; every one used to panic or misbehave.
	for _, tc := range []struct {
		name string
		hash string
	}{
		{"t=0", `$argon2id$v=19$m=65536,t=0,p=4$c29tZXNhbHR2YWx1ZQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGg`},
		{"p=0", `$argon2id$v=19$m=65536,t=3,p=0$c29tZXNhbHR2YWx1ZQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGg`},
		{"empty key", `$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHR2YWx1ZQ$`},
		{"garbage", `not-a-phc-string`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := pgtest.Open(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			if _, err := db.Pool().Exec(ctx,
				`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', $1)`, tc.hash,
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

			// Before the fix this panicked; the barrier would now turn that
			// into SERVERBUG, so assert on the *correct* outcome:
			// an ordinary authentication failure.
			err := sess.Login("alice", "whatever")
			if err == nil {
				t.Fatal("Login succeeded against a malformed stored hash")
			}
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) {
				t.Fatalf("err = %T (%v), want *imap.Error", err, err)
			}
			if imapErr.Code != imap.ResponseCodeAuthenticationFailed {
				t.Errorf("Code = %q, want AUTHENTICATIONFAILED (got %v)", imapErr.Code, imapErr)
			}
			if sess.mailboxID != 0 {
				t.Error("session became authenticated against a malformed hash")
			}
		})
	}
}

// TestVerifyPasswordBoundsMatchLoginPath pins the contract the Login path
// relies on: a malformed stored hash is a non-nil error, never a panic and
// never a true.
func TestVerifyPasswordBoundsMatchLoginPath(t *testing.T) {
	ok, err := auth.VerifyPassword("whatever",
		`$argon2id$v=19$m=65536,t=0,p=4$c29tZXNhbHR2YWx1ZQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGg`)
	if ok {
		t.Error("malformed hash authenticated")
	}
	if err == nil {
		t.Error("malformed hash returned a nil error; Login would treat it as a plain mismatch")
	}
}
