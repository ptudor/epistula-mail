package imapsess

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryTxPanicReturnsPooledConnection(t *testing.T) {
	s := mutationFixture(t)
	func() {
		defer func() {
			if p := recover(); p != "test panic" {
				t.Fatalf("panic=%v", p)
			}
		}()
		_ = retryTx(context.Background(), s.be.Pool.Begin, func(pgx.Tx) error { panic("test panic") })
	}()
	if n := s.be.Pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("panic leaked %d acquired connections", n)
	}
}

type unknownCommitTx struct{ pgx.Tx }

func (*unknownCommitTx) Commit(context.Context) error   { return &pgconn.PgError{Code: "40003"} }
func (*unknownCommitTx) Rollback(context.Context) error { return nil }

func TestRetryTxUnknownCommitDoesNotReplay(t *testing.T) {
	calls := 0
	err := retryTx(context.Background(), func(context.Context) (pgx.Tx, error) { return &unknownCommitTx{}, nil }, func(pgx.Tx) error { calls++; return nil })
	if calls != 1 || !isTxAborted(err) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	for _, code := range []string{"40001", "40P01", "40003", "40000", "23505"} {
		want := code == "40001" || code == "40P01"
		if got := isRetryableTxError(&pgconn.PgError{Code: code}); got != want {
			t.Fatalf("%s retryable=%v", code, got)
		}
	}
}
