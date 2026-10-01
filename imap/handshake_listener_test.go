package main

import (
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/imap/imapsess"
)

// hsTestListener stands up a real TLS listener wrapped by the
// handshake-timeout enforcement, with a short deadline suitable for tests.
func hsTestListener(t *testing.T, timeout time.Duration) (net.Listener, string) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeSelfSigned(t, "imap.test.invalid", certPath, keyPath)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := imapsess.NewTLSHandshakeListener(
		tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}}),
		timeout, nil,
	)
	t.Cleanup(func() { _ = ln.Close() })
	return ln, raw.Addr().String()
}

func TestTLSHandshakeListenerDropsStalledHandshake(t *testing.T) {
	ln, addr := hsTestListener(t, 200*time.Millisecond)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	// Connect and send nothing — never start the TLS handshake.
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// The server must cut the connection at the handshake deadline; the
	// client observes that as EOF/reset on its next read.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	_, readErr := client.Read(buf)
	if readErr == nil {
		t.Fatal("read succeeded; expected the server to drop the stalled handshake")
	}
	var nerr net.Error
	if errors.As(readErr, &nerr) && nerr.Timeout() {
		t.Fatal("client read timed out; server never dropped the stalled handshake")
	}

	select {
	case <-accepted:
		t.Fatal("a never-handshaken connection must not surface from Accept")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTLSHandshakeListenerDeliversCompletedHandshake(t *testing.T) {
	ln, addr := hsTestListener(t, 2*time.Second)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer client.Close()

	select {
	case c := <-accepted:
		if _, ok := c.(*tls.Conn); !ok {
			t.Errorf("Accept returned %T, want *tls.Conn (go-imap relies on the concrete type)", c)
		}
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("handshaken connection never surfaced from Accept")
	}
}
