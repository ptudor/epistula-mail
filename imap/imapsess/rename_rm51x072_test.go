package imapsess

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestRenameSelectedDescendantKeepsConnection(t *testing.T) {
	s, _ := appendFixture(t)
	defer s.Close()
	mustCreateFolder(t, s, "Archive/2026")
	if _, err := s.Append("Archive/2026", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatal(err)
	}
	c := fetchWire(t, s)
	for _, cmd := range []string{"SELECT Archive/2026", "RENAME Archive Old", "NOOP", "UID FETCH 1:* (UID)"} {
		c.send("r " + cmd)
		if got := c.tagged("r"); !strings.HasPrefix(got, "r OK") {
			t.Fatalf("%s: %s", cmd, got)
		}
	}
}

func TestRenameRechecksSourceUnderLock(t *testing.T) {
	s, _ := appendFixture(t)
	defer s.Close()
	mustCreateFolder(t, s, "X/child")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := s.be.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if err := lockMailbox(ctx, a, s.mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `UPDATE folders SET name='Y'||substring(name from 2) WHERE mailbox_id=$1 AND (name='X' OR name LIKE 'X/%')`, s.mailboxID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Rename("X", "Z", nil) }()
	waitForMailboxLockWait(t, ctx, s, done)
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ie *imap.Error
	if err := <-done; !errors.As(err, &ie) || ie.Code != imap.ResponseCodeNonExistent {
		t.Fatalf("stale rename: %v", err)
	}
	if got := folderNames(t, s); !reflect.DeepEqual(got, []string{"Y", "Y/child"}) {
		t.Fatal(got)
	}
}
