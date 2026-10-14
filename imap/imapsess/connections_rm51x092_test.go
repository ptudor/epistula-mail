package imapsess

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGlobalConnectionCeilingDistinctAddresses(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := NewLimitedListener(raw, NewPerIPLimiter(10), nil, 2)
	defer listener.Close()
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	for i := 1; i <= 3; i++ {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(fmt.Sprintf("127.0.0.%d", i))}, Timeout: time.Second}
		c, err := d.Dial("tcp", listener.Addr().String())
		if errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Logf("source alias 127.0.0.%d unavailable; verifying ceiling with primary loopback", i)
			c, err = net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		}
		if err != nil {
			t.Fatalf("dial source 127.0.0.%d: %v", i, err)
		}
		defer c.Close()
		if i < 3 {
			select {
			case server := <-accepted:
				defer server.Close()
			case <-time.After(time.Second):
				t.Fatal("not accepted")
			}
		} else {
			c.SetReadDeadline(time.Now().Add(time.Second))
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "* BYE Too many connections") {
				t.Fatalf("refusal %q %v", line, err)
			}
		}
	}
}

func TestConcurrentHandshakeBound(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := NewTLSHandshakeListener(tls.NewListener(raw, &tls.Config{}), 200*time.Millisecond, nil, 2)
	defer listener.Close()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	deadline := time.Now().Add(time.Second)
	for len(listener.slots) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(listener.slots) != 2 {
		t.Fatal("handshakes not started")
	}
	excess, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer excess.Close()
	excess.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := excess.Read(b[:]); err == nil {
		t.Fatal("excess handshake was not closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("excess handshake remained open")
	}

	for len(listener.slots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(listener.slots) != 0 {
		t.Fatal("handshake slots leaked")
	}
}
