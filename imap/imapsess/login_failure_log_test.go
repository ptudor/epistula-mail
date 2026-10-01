package imapsess

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestLoginFailuresAreLogged: a refused LOGIN leaves a line saying why, with
// the username and the client's address and never the password. Apple Mail
// configured without a username retried an empty LOGIN every half minute and
// the log said nothing, so the account was simply "not working".
func TestLoginFailuresAreLogged(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hash, err := auth.HashPassword("rightpw", auth.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', $1)`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash, disabled_at) VALUES ('gone', $1, now())`, hash); err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("u", 200)
	for _, tc := range []struct {
		name, username, password string
		want                     []string
	}{
		{"wrong password", "alice", "secret-guess-1", []string{"level=WARN", `msg="login failed"`, `reason="wrong password"`, "mailbox=alice", "ip=1.2.3.4"}},
		{"disabled", "gone", "rightpw", []string{"level=WARN", `reason="mailbox disabled"`, "mailbox=gone"}},
		{"unknown", "ptudor@example.invalid", "secret-guess-2", []string{"level=INFO", `reason="no such mailbox"`, "username=ptudor@example.invalid"}},
		{"empty username", "", "secret-guess-3", []string{"level=INFO", `reason="username missing"`, "ip=1.2.3.4"}},
		{"empty password", "alice", "", []string{"level=INFO", `reason="password missing"`, "username=alice"}},
		{"long username", long, "secret-guess-4", []string{"username=" + strings.Repeat("u", maxLoggedUsername) + "..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			be := &Backend{
				Pool:        db.Pool(),
				BlobStore:   blob.NewStore(t.TempDir()),
				Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
				StmtTimeout: 10 * time.Second,
			}
			sess := be.NewSession()
			sess.remoteIP = "1.2.3.4"
			if err := sess.Login(tc.username, tc.password); err == nil {
				t.Fatal("Login succeeded")
			}
			out := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("log lacks %q:\n%s", w, out)
				}
			}
			if tc.password != "" && strings.Contains(out, tc.password) {
				t.Fatalf("the password reached the log:\n%s", out)
			}
			if strings.Contains(out, strings.Repeat("u", maxLoggedUsername+1)) {
				t.Fatalf("an over-long username was logged whole")
			}
		})
	}

	// A successful login is still the one "login ok" line, with no failure.
	var buf bytes.Buffer
	be := &Backend{Pool: db.Pool(), BlobStore: blob.NewStore(t.TempDir()),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)), StmtTimeout: 10 * time.Second}
	sess := be.NewSession()
	sess.remoteIP = "1.2.3.4"
	if err := sess.Login("alice", "rightpw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "login failed") || !strings.Contains(out, `msg="login ok"`) {
		t.Fatalf("successful login logged:\n%s", out)
	}
}
