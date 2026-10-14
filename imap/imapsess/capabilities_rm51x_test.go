package imapsess

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

func featureWire(t *testing.T, fixture *Session) (*bufio.Reader, net.Conn) {
	t.Helper()
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := fixture.be.NewSessionForIMAPConn(c)
			s.mailboxID = fixture.mailboxID
			s.mailboxName = fixture.mailboxName
			s.tenant = fixture.tenant
			return s, &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListExtended: {}, imap.CapListStatus: {}, imap.CapChildren: {}, imap.CapIdle: {}},
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
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return br, conn
}

func featureCommand(t *testing.T, br *bufio.Reader, conn net.Conn, tag, command string) string {
	t.Helper()
	fmt.Fprintf(conn, "%s %s\r\n", tag, command)
	var out strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return out.String()
		}
	}
}

func TestListStatusAndChildrenOnWire(t *testing.T) {
	fixture := mutationFixture(t)
	if err := fixture.Create("Parent/Child", nil); err != nil {
		t.Fatal(err)
	}
	br, conn := featureWire(t, fixture)
	got := featureCommand(t, br, conn, "l1", `LIST "" "*" RETURN (STATUS (MESSAGES UNSEEN) CHILDREN)`)
	if !strings.Contains(got, `\HasNoChildren`) || !strings.Contains(got, `\HasChildren`) || !strings.Contains(got, "* STATUS INBOX (MESSAGES 3 UNSEEN 3)") || !strings.Contains(got, "l1 OK") {
		t.Fatal(got)
	}
	// The matching LIST must precede its STATUS record.
	if strings.Index(got, "INBOX\r\n") > strings.Index(got, "* STATUS INBOX") {
		t.Fatal(got)
	}
}

func TestAppendLimitRefusesBeforeContinuation(t *testing.T) {
	fixture := mutationFixture(t)
	fixture.be.MaxAppendBytes = 52428800
	br, conn := featureWire(t, fixture)
	if got := featureCommand(t, br, conn, "c1", "CAPABILITY"); !strings.Contains(got, "APPENDLIMIT=52428800") {
		t.Fatal(got)
	}
	fmt.Fprint(conn, "a1 APPEND INBOX {52428801}\r\n")
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "a1 NO [TOOBIG]") {
		t.Fatalf("unexpected continuation/refusal: %s", line)
	}
}

func TestShutdownNotifiesIdleOnWire(t *testing.T) {
	fixture := mutationFixture(t)
	br, conn := featureWire(t, fixture)
	fixture.Close() // only the wire session remains active
	if got := featureCommand(t, br, conn, "s1", "SELECT INBOX"); !strings.Contains(got, "s1 OK") {
		t.Fatal(got)
	}
	fmt.Fprint(conn, "i1 IDLE\r\n")
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE %q %v", line, err)
	}
	start := time.Now()
	fixture.be.BeginShutdown()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "* BYE server shutting down") {
			break
		}
	}
	deadline := start.Add(time.Second)
	for fixture.be.ActiveSessions() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fixture.be.ActiveSessions() != 0 {
		t.Fatal("IDLE session did not close during drain")
	}
	t.Logf("BYE and session closure in %s", time.Since(start))
}
