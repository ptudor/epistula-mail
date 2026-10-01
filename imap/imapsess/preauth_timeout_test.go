package imapsess

import (
	"net"
	"testing"
	"time"
)

// TestPreAuthTimeoutClosesUnauthenticated is the R-039 regression: an
// unauthenticated connection is closed once PreAuthTimeout elapses. The timer
// is independent of any per-command read deadline, so command traffic cannot
// keep the connection alive past the budget. Uses an in-memory net.Pipe — no
// DB and no sockets.
func TestPreAuthTimeoutClosesUnauthenticated(t *testing.T) {
	be := &Backend{PreAuthTimeout: 60 * time.Millisecond}
	client, server := net.Pipe()
	defer client.Close()

	_ = be.NewSessionForNetConn(server) // starts the pre-auth timer on `server`

	// A blocking Read on the peer must return an error once the timer closes
	// `server`; a generous deadline guards against a hung test.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Fatal("connection was not closed after PreAuthTimeout elapsed")
	}
}

// TestPreAuthTimeoutSparesAuthenticated: once Login flips `authenticated` and
// stops the timer, the connection survives past the budget.
func TestPreAuthTimeoutSparesAuthenticated(t *testing.T) {
	be := &Backend{PreAuthTimeout: 60 * time.Millisecond}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	s := be.NewSessionForNetConn(server)
	// What a successful Login does.
	s.authenticated.Store(true)
	if s.preAuthTimer != nil {
		s.preAuthTimer.Stop()
	}

	time.Sleep(150 * time.Millisecond) // well past the budget

	// The conn is still open: a Read blocks to our deadline (timeout error)
	// rather than returning immediately from a closed pipe.
	_ = client.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	buf := make([]byte, 1)
	_, err := client.Read(buf)
	ne, ok := err.(net.Error)
	if !ok || !ne.Timeout() {
		t.Fatalf("expected a read timeout on the still-open conn, got %v", err)
	}
}
