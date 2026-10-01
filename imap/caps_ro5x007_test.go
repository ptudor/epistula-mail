package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// capsProbeSession is a stub that authenticates and records whether the
// library routed a CREATE ... (USE (\Archive)) parameter through to us.
type capsProbeSession struct {
	imapserver.Session // nil: only the methods below are exercised
	created            chan *imap.CreateOptions
}

func (s *capsProbeSession) Close() error                          { return nil }
func (s *capsProbeSession) Login(username, password string) error { return nil }

// Namespace and Move satisfy imapserver.SessionIMAP4rev2, which the library
// requires of any session when IMAP4rev2 is advertised. The real
// imapsess.Session implements both; the stub needs them only to be accepted.
func (s *capsProbeSession) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}}}, nil
}

func (s *capsProbeSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	return nil
}

func (s *capsProbeSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error { return nil }

func (s *capsProbeSession) Create(mailbox string, options *imap.CreateOptions) error {
	select {
	case s.created <- options:
	default:
	}
	return nil
}

// capsDialer starts a server with the production capability set and returns a
// connected client plus the channel recording CREATE options.
func capsDialer(t *testing.T) (*bufio.Reader, net.Conn, chan *imap.CreateOptions) {
	t.Helper()
	created := make(chan *imap.CreateOptions, 4)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &capsProbeSession{created: created}, &imapserver.GreetingData{PreAuth: false}, nil
		},
		Caps:         advertisedCaps(),
		InsecureAuth: true,
		Logger:       log.New(os.Stderr, "imapsrv: ", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return br, conn, created
}

// runCmd issues one tagged command and returns the untagged lines plus the
// tagged completion line.
func runCmd(t *testing.T, br *bufio.Reader, conn net.Conn, tag, cmd string) ([]string, string) {
	t.Helper()
	fmt.Fprintf(conn, "%s %s\r\n", tag, cmd)
	var untagged []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" ") {
			return untagged, line
		}
		untagged = append(untagged, line)
	}
}

// TestAdvertisedCapabilitiesIncludeImplementedOnes is the RO5X-007
// verification, done on the wire.
//
// IDLE, LIST-EXTENDED, and SPECIAL-USE are all implemented and were all
// documented as shipped, but none was advertised — so every MUA polled
// instead of using the pg_notify push path.
func TestAdvertisedCapabilitiesIncludeImplementedOnes(t *testing.T) {
	br, conn, _ := capsDialer(t)

	// Pre-auth, the library advertises only what is usable before login.
	// The extension set appears once authenticated — that is normal IMAP
	// behaviour and where a client actually reads it.
	untagged, done := runCmd(t, br, conn, "a1", "CAPABILITY")
	if !strings.HasPrefix(done, "a1 OK") {
		t.Fatalf("CAPABILITY failed: %s", done)
	}
	t.Logf("pre-auth CAPABILITY: %s", strings.Join(untagged, " "))

	if _, done := runCmd(t, br, conn, "a2", "LOGIN user pass"); !strings.HasPrefix(done, "a2 OK") {
		t.Fatalf("LOGIN failed: %s", done)
	}
	untagged, done = runCmd(t, br, conn, "a3", "CAPABILITY")
	if !strings.HasPrefix(done, "a3 OK") {
		t.Fatalf("post-auth CAPABILITY failed: %s", done)
	}
	caps := strings.Join(untagged, " ")
	t.Logf("post-auth CAPABILITY: %s", caps)

	// The three RO5X-007 additions, plus the four that were already correct.
	for _, want := range []string{
		"IDLE", "LIST-EXTENDED", "SPECIAL-USE",
		"MOVE", "UIDPLUS", "ESEARCH", "NAMESPACE",
	} {
		if !strings.Contains(caps, want) {
			t.Errorf("post-auth CAPABILITY is missing %s: %s", want, caps)
		}
	}

	// Must stay unadvertised.
	for _, unwanted := range []string{"COMPRESS=DEFLATE", "CONDSTORE", "QRESYNC"} {
		if strings.Contains(caps, unwanted) {
			t.Errorf("CAPABILITY advertises %s, which is not implemented: %s", unwanted, caps)
		}
	}
}

// TestLibraryAutoAdvertisedCapabilities records which capabilities the
// library injects on its own (RO5X-043). Anything listed here
// needs no entry in advertisedCaps().
func TestLibraryAutoAdvertisedCapabilities(t *testing.T) {
	br, conn, _ := capsDialer(t)
	untagged, _ := runCmd(t, br, conn, "a1", "CAPABILITY")
	caps := strings.Join(untagged, " ")

	// These are never named in advertisedCaps() yet must appear.
	for _, want := range []string{"IMAP4rev1", "LITERAL-", "AUTH=PLAIN"} {
		if !strings.Contains(caps, want) {
			t.Errorf("expected the library to auto-advertise %s; it did not: %s", want, caps)
		}
	}
}

// TestCreateSpecialUseIsRoutedAndHandled records how the library routes a
// CREATE USE parameter (RO5X-043) and pins the behaviour that requires.
//
// go-imap parses `CREATE ... (USE (\Archive))` and hands it to Session.Create
// in options.SpecialUse **regardless of whether SPECIAL-USE is advertised** —
// confirmed by probing with the capability removed. So this was never
// something advertising SPECIAL-USE switched on; the server has always been
// answering OK while discarding the attribute, and LIST (which reads
// folders.special_use) would then never report it.
//
// Create now validates and persists it, so the OK is truthful.
func TestCreateSpecialUseIsRoutedAndHandled(t *testing.T) {
	br, conn, created := capsDialer(t)

	caps, _ := runCmd(t, br, conn, "a1", "CAPABILITY")
	if strings.Contains(strings.Join(caps, " "), "CREATE-SPECIAL-USE") {
		t.Error("CREATE-SPECIAL-USE should not be advertised")
	}
	if _, done := runCmd(t, br, conn, "a2", "LOGIN user pass"); !strings.HasPrefix(done, "a2 OK") {
		t.Fatalf("LOGIN failed: %s", done)
	}

	_, done := runCmd(t, br, conn, "a3", `CREATE "Archive" (USE (\Archive))`)
	if !strings.HasPrefix(done, "a3 OK") {
		t.Fatalf("CREATE with USE failed: %s", done)
	}
	select {
	case opts := <-created:
		if opts == nil || len(opts.SpecialUse) != 1 || opts.SpecialUse[0] != imap.MailboxAttrArchive {
			t.Fatalf("options = %+v, want SpecialUse=[\\Archive]", opts)
		}
		t.Logf("library routed SpecialUse=%v into Session.Create", opts.SpecialUse)
	case <-time.After(2 * time.Second):
		t.Fatal("CREATE never reached Session.Create")
	}
}

// TestPlainCreateStillWorks guards against the SPECIAL-USE advertisement
// breaking ordinary CREATE.
func TestPlainCreateStillWorks(t *testing.T) {
	br, conn, created := capsDialer(t)
	if _, done := runCmd(t, br, conn, "a1", "LOGIN user pass"); !strings.HasPrefix(done, "a1 OK") {
		t.Fatalf("LOGIN failed: %s", done)
	}
	_, done := runCmd(t, br, conn, "a2", `CREATE "Archive/2026"`)
	if !strings.HasPrefix(done, "a2 OK") {
		t.Fatalf("plain CREATE failed: %s", done)
	}
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Error("plain CREATE never reached Session.Create")
	}
}
