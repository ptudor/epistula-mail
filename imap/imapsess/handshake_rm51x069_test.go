package imapsess

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

type transientAcceptListener struct {
	net.Listener
	failed bool
}

func (l *transientAcceptListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

func TestTLSHandshakeListenerRetriesTemporaryAccept(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := NewTLSHandshakeListener(&transientAcceptListener{Listener: inner}, time.Second, nil)
	defer l.Close()
	peer, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Accept after EMFILE: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not recover after EMFILE")
	}
	l.Close()
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v", err)
	}
}
