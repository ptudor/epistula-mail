package imapsess

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

func verificationWire(t *testing.T, fixture *Session) func(string) string {
	t.Helper()
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := fixture.be.NewSession()
			s.mailboxID, s.mailboxName, s.tenant = fixture.mailboxID, fixture.mailboxName, fixture.tenant
			return s, &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {}, imap.CapIdle: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(ln)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	seq := 0
	return func(command string) string {
		t.Helper()
		seq++
		tag := fmt.Sprintf("v%d", seq)
		fmt.Fprintf(conn, "%s %s\r\n", tag, command)
		var out strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("%s: %v", command, err)
			}
			out.WriteString(line)
			if strings.HasPrefix(line, tag+" ") {
				if !strings.HasPrefix(line, tag+" OK") {
					t.Fatalf("%s: %s", command, out.String())
				}
				return out.String()
			}
		}
	}
}

func TestVerificationExpungeUsesWireSequence(t *testing.T) {
	s := mutationFixture(t)
	a, b := verificationWire(t, s), verificationWire(t, s)
	a("SELECT INBOX")
	b("SELECT INBOX")
	b("UID STORE 1 +FLAGS.SILENT (\\Deleted)")
	b("UID EXPUNGE 1")
	// A still knows [1,2,3]. Its own removal of UID 3 must say sequence 3;
	// the peer's pending removal of UID 1 is then delivered once by Poll.
	got := a("UID EXPUNGE 3")
	if !strings.Contains(got, "* 3 EXPUNGE\r\n") || !strings.Contains(got, "* 1 EXPUNGE\r\n") || strings.Contains(got, "* 2 EXPUNGE") {
		t.Fatalf("wrong expunge identities: %s", got)
	}
	if got := a("NOOP"); strings.Contains(got, "EXPUNGE") {
		t.Fatalf("duplicate update: %s", got)
	}
}

func TestVerificationSearchUsesWireSequence(t *testing.T) {
	s := mutationFixture(t)
	a, b := verificationWire(t, s), verificationWire(t, s)
	a("SELECT INBOX")
	b("SELECT INBOX")
	b("UID STORE 1 +FLAGS.SILENT (\\Deleted)")
	b("UID EXPUNGE 1")
	if got := a("SEARCH UID 2"); !strings.Contains(got, "* SEARCH 2\r\n") {
		t.Fatalf("SEARCH renumbered silently: %s", got)
	}
	if got := a("UID SEARCH 2"); !strings.Contains(got, "* SEARCH 2\r\n") {
		t.Fatalf("SEARCH criterion renumbered silently: %s", got)
	}
	a("NOOP")
	if got := a("SEARCH UID 2"); !strings.Contains(got, "* SEARCH 1\r\n") {
		t.Fatalf("SEARCH did not follow announced expunge: %s", got)
	}
}

func TestVerificationCopyPreservesSentDate(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	if _, err := s.be.Pool.Exec(ctx, `UPDATE messages SET sent_date_local = '2026-09-04' WHERE folder_id = $1 AND uid = 1`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Copy(imap.UIDSetNum(1), "Archive"); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := s.be.Pool.QueryRow(ctx, `SELECT COALESCE(m.sent_date_local = '2026-09-04', false) FROM messages m JOIN folders f ON f.id = m.folder_id WHERE f.mailbox_id = $1 AND f.name = 'Archive'`, s.mailboxID).Scan(&correct); err != nil {
		t.Fatal(err)
	}
	if !correct {
		t.Fatal("COPY dropped the sender's calendar date")
	}
}

func TestVerificationIdleWithoutSelectionCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stop := make(chan struct{})
	s := &Session{}
	go func() { done <- s.idleConsume(ctx, nil, stop) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(stop)
		<-done
		t.Fatal("unselected IDLE ignored session cancellation")
	}
}

func TestVerificationPermanentFlagsOnWire(t *testing.T) {
	s := mutationFixture(t)
	if err := s.Create("Empty", nil); err != nil {
		t.Fatal(err)
	}
	a := verificationWire(t, s)
	if got := a("SELECT Empty"); !strings.Contains(got, `\*`) {
		t.Fatalf("empty writable folder omitted keyword creation: %s", got)
	}
	a("SELECT INBOX")
	a("UID STORE 1 +FLAGS.SILENT (verify-keyword)")
	b := verificationWire(t, s)
	if got := b("SELECT INBOX"); !strings.Contains(got, "verify-keyword") || !strings.Contains(got, `\*`) {
		t.Fatalf("reconnected writable selection lost keyword capability: %s", got)
	}
	if got := b("UID FETCH 1 (FLAGS)"); !strings.Contains(got, "verify-keyword") {
		t.Fatalf("keyword did not persist: %s", got)
	}
	if got := b("EXAMINE INBOX"); !strings.Contains(got, "[PERMANENTFLAGS ()]") {
		t.Fatalf("read-only selection advertised permanent mutation: %s", got)
	}
}
