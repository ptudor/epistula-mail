package imapsess

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/storage"
)

// TestResyncedFolderClosesSelectedSession: epistula-database admin
// folder-resync gives a folder a new UIDVALIDITY so every client re-reads it.
// A session with the folder selected must stop answering for the old
// generation and close, so its client reconnects, and the next SELECT must
// report the new UIDVALIDITY, which tells the client to discard its cache.
func TestResyncedFolderClosesSelectedSession(t *testing.T) {
	sess, _ := appendFixture(t)
	defer sess.Close()
	mustCreateFolder(t, sess, "INBOX")
	if _, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatal(err)
	}
	c := fetchWire(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := storage.NewFromPool(sess.be.Pool, storage.Config{StatementTimeout: 5 * time.Second})
	r, err := db.ResyncFolder(ctx, sess.mailboxID, "INBOX", false)
	if err != nil {
		t.Fatalf("ResyncFolder: %v", err)
	}
	if r.After <= r.Before {
		t.Fatalf("resync = %+v, want a greater UIDVALIDITY", r)
	}

	// The session may say NO or BYE first, but it must not answer OK for the
	// old generation, and the connection must end.
	c.send("n NOOP")
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("session neither answered nor closed after its folder changed generation")
			}
			break // closed: the client reconnects and SELECTs afresh
		}
		if strings.HasPrefix(line, "n OK") {
			t.Fatalf("NOOP after resync answered %q; the session kept serving the old generation", line)
		}
	}

	fresh := fetchWire(t, sess)
	fresh.send("x SELECT INBOX")
	want := fmt.Sprintf("[UIDVALIDITY %d]", r.After)
	var seen bool
	fresh.until(func(l string) bool {
		if strings.Contains(l, want) {
			seen = true
		}
		return strings.HasPrefix(l, "x ")
	})
	if !seen {
		t.Errorf("SELECT after resync did not report %s", want)
	}
}
