package imapsess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSearchTimeoutResponse(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}} {
		var response *imap.Error
		if !errors.As(searchQueryError(cause), &response) || response.Type != imap.StatusResponseTypeNo || response.Code != imap.ResponseCodeLimit {
			t.Fatalf("response=%v", response)
		}
	}
	var other *imap.Error
	if !errors.As(searchQueryError(errors.New("connection lost")), &other) || other.Code != "" {
		t.Fatalf("unrelated error=%v", other)
	}
}

func TestSearchHungQueryReportsLimit(t *testing.T) {
	sess := mutationFixture(t)
	if _, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := sess.be.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	sess.be.StmtTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err = sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{Body: []string{"needle"}}, nil)
	var response *imap.Error
	if !errors.As(err, &response) || response.Code != imap.ResponseCodeLimit {
		t.Fatalf("response=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("search failed to cancel")
	}
}
