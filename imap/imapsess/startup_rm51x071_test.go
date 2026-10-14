package imapsess

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestDeferredStartupCheckRecoversWithoutRestart(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("right", auth.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('alice',$1)`, hash); err != nil {
		t.Fatal(err)
	}

	be := &Backend{
		Pool:        db.Pool(),
		BlobStore:   blob.NewStore(t.TempDir()),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		StmtTimeout: 10 * time.Second,
		AuthBudget:  NewAuthBudgetMiB(256),
	}
	calls := 0
	be.ConfigureStartupCheck(func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("postgres still starting")
		}
		return nil
	}, false)

	first := be.NewSession()
	err = first.Login("alice", "right")
	first.Close()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeUnavailable {
		t.Fatalf("first login = %v, want NO [UNAVAILABLE]", err)
	}

	second := be.NewSession()
	defer second.Close()
	if err := second.Login("alice", "right"); err != nil {
		t.Fatalf("login after recovery: %v", err)
	}
	if calls != 2 {
		t.Fatalf("startup check calls = %d, want 2", calls)
	}
	if err := be.EnsureStartupReady(ctx); err != nil || calls != 2 {
		t.Fatalf("successful startup check was not cached: calls=%d err=%v", calls, err)
	}
}
