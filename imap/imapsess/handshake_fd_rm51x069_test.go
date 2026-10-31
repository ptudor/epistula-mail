//go:build darwin || linux || freebsd

package imapsess

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

type fdExhaustionLog struct{ hit chan struct{} }

func (l *fdExhaustionLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *fdExhaustionLog) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && errors.Is(err, syscall.EMFILE) {
			select {
			case l.hit <- struct{}{}:
			default:
			}
		}
		return true
	})
	return nil
}
func (l *fdExhaustionLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *fdExhaustionLog) WithGroup(string) slog.Handler      { return l }

// Exhaust real descriptors in a subprocess so other tests keep their limits.
func TestTLSHandshakeListenerRecoversRealFDExhaustion(t *testing.T) {
	if os.Getenv("EPISTULA_TEST_FD_EXHAUSTION") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTLSHandshakeListenerRecoversRealFDExhaustion$")
		cmd.Env = append(os.Environ(), "EPISTULA_TEST_FD_EXHAUSTION=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fd exhaustion child: %v\n%s", err, out)
		}
		return
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = min(limit.Max, 64)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	// One queued connection is what accept fails on. Every other spare
	// descriptor is held by /dev/null rather than by more connections: a
	// backlog of closed peers would be accepted during recovery and could
	// exhaust descriptors a second time, and on BSD an accept that fails
	// with EMFILE discards the queued connection, which could then be the
	// fresh one this test waits for (the macOS runner saw exactly that).
	queued, err := net.DialTimeout("tcp", inner.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer queued.Close()
	var held []*os.File
	defer func() {
		for _, f := range held {
			_ = f.Close()
		}
	}()
	for {
		f, err := os.Open(os.DevNull)
		if errors.Is(err, syscall.EMFILE) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	log := &fdExhaustionLog{hit: make(chan struct{}, 1)}
	l := NewTLSHandshakeListener(inner, time.Second, slog.New(log))
	defer l.Close()
	select {
	case <-log.hit:
	case <-time.After(3 * time.Second):
		t.Fatal("real accept did not report EMFILE")
	}
	for _, f := range held {
		_ = f.Close()
	}
	held = nil
	// Closed now so that, where the kernel kept it queued, it is accepted
	// and read as EOF at once instead of holding the drain for a deadline.
	_ = queued.Close()
	// BSD may have discarded the queued connection on EMFILE; recovery must
	// admit a fresh connection after descriptors are released.
	recovered, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				result <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			_, err = io.ReadFull(conn, b[:])
			_ = conn.Close()
			// The queued connection, if it survived, reads as EOF; only the
			// fresh client sends x.
			if err == nil && b[0] == 'x' {
				result <- nil
				return
			}
		}
	}()

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accept did not recover after descriptors released")
	}
}
