package imapsess

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/ptudor/epistula-mail/database/blob"
)

func fetchWire(t *testing.T, fixture *Session) *idleWire {
	t.Helper()
	be := fixture.be
	mailboxID, mailboxName, tenant := fixture.mailboxID, fixture.mailboxName, fixture.tenant
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := be.NewSessionForNetConn(c.NetConn())
			s.mailboxID, s.mailboxName, s.tenant = mailboxID, mailboxName, tenant
			return s, &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}},
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
	c := &idleWire{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.until(func(s string) bool { return strings.HasPrefix(s, "* PREAUTH") })
	c.send("s SELECT INBOX")
	if got := c.tagged("s"); !strings.HasPrefix(got, "s OK") {
		t.Fatal(got)
	}
	return c
}

func waitFetchSessions(t *testing.T, be *Backend, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for be.ActiveSessions() != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := be.ActiveSessions(); got != want {
		t.Fatalf("active sessions = %d, want %d", got, want)
	}
}

func TestFetchMissingBlobReleasesResponseWriter(t *testing.T) {
	sess, store := appendFixture(t)
	defer sess.Close()
	mustCreateFolder(t, sess, "INBOX")
	if _, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatal(err)
	}
	var sha string
	var date time.Time
	if err := sess.be.Pool.QueryRow(context.Background(), `SELECT encode(raw_sha256,'hex'), raw_blob_date FROM messages`).Scan(&sha, &date); err != nil {
		t.Fatal(err)
	}
	path, err := store.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(date), sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c := fetchWire(t, sess)
	for i, section := range []string{"BODY.PEEK[]", "BODY.PEEK[HEADER]", "BODY.PEEK[1]"} {
		tag := fmt.Sprintf("f%d", i)
		c.send(tag + " FETCH 1 (" + section + ")")
		line, err := c.r.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, tag+" NO [SERVERBUG]") {
			t.Fatalf("FETCH emitted %q, %v; want clean tagged NO", line, err)
		}
		c.send("n NOOP")
		if got := c.tagged("n"); !strings.HasPrefix(got, "n OK") {
			t.Fatal(got)
		}
	}
	c.conn.Close()
	waitFetchSessions(t, sess.be, 1)
	sess.Close()
	waitFetchSessions(t, sess.be, 0)
}

func TestFetchDisconnectDuringLiteralReleasesSession(t *testing.T) {
	sess, _ := appendFixture(t)
	defer sess.Close()
	mustCreateFolder(t, sess, "INBOX")
	raw := []byte(appendRawMsg + strings.Repeat("body\r\n", 4*1024*1024))
	if _, err := sess.Append("INBOX", newLiteral(raw), nil); err != nil {
		t.Fatal(err)
	}
	baseline := runtime.NumGoroutine()
	for i := 0; i < 4; i++ {
		c := fetchWire(t, sess)
		c.send("f FETCH 1 (BODY.PEEK[])")
		line := c.until(func(s string) bool { return strings.Contains(s, "FETCH (") })
		if !strings.Contains(line, fmt.Sprintf("{%d}", len(raw))) {
			t.Fatalf("missing literal announcement: %s", line)
		}
		c.conn.Close()
		waitFetchSessions(t, sess.be, 1)
	}
	// The four test servers still own one accept goroutine apiece.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+4 {
		t.Fatalf("goroutines = %d, baseline %d plus four listeners", got, baseline)
	}
	sess.Close()
	waitFetchSessions(t, sess.be, 0)
}
