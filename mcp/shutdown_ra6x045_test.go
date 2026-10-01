package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestShutdownWaitsForInFlightToolCalls is the RA6X-045 regression.
//
// Shutdown closes the listener first, so ListenAndServe returns ErrServerClosed
// almost immediately while Shutdown is still waiting for in-flight handlers.
// Returning at that moment ends the process regardless of what those handlers
// are doing, so an annotation write in progress was cut off despite the
// advertised ten-second drain.
func TestShutdownWaitsForInFlightToolCalls(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var completed atomic.Bool

	srv := &http.Server{
		Addr: freeAddr(t),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			completed.Store(true)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("done"))
		}),
	}
	shutdownCtx, stop := context.WithCancel(context.Background())
	defer stop()

	served := make(chan error, 1)
	go func() { served <- serveWithDrain(srv, shutdownCtx, stop, 10*time.Second) }()
	waitForListener(t, srv.Addr)

	// One request, blocked inside the handler.
	inflight := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + srv.Addr + "/")
		if err == nil {
			resp.Body.Close()
		}
		inflight <- err
	}()
	waitForHandler(t, entered)

	// SIGTERM equivalent.
	stop()

	// serveWithDrain must NOT have returned yet: the handler is still running.
	select {
	case err := <-served:
		t.Fatalf("serveWithDrain returned while a handler was in flight (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if completed.Load() {
		t.Fatal("the handler finished on its own; the test is not exercising the drain")
	}

	// Release within the window: the call completes, then the server returns.
	close(release)
	if err := <-inflight; err != nil {
		t.Errorf("the in-flight request failed: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveWithDrain = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveWithDrain did not return after the drain finished")
	}
	if !completed.Load() {
		t.Error("the in-flight handler never completed")
	}
}

// TestShutdownStopsForAHandlerThatNeverReturns pins the bounded hard stop: a
// permanently blocked handler must not hang the process past the drain
// deadline.
func TestShutdownStopsForAHandlerThatNeverReturns(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	entered := make(chan struct{})

	srv := &http.Server{
		Addr: freeAddr(t),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-block
		}),
	}
	shutdownCtx, stop := context.WithCancel(context.Background())
	defer stop()

	served := make(chan error, 1)
	go func() { served <- serveWithDrain(srv, shutdownCtx, stop, 500*time.Millisecond) }()
	waitForListener(t, srv.Addr)

	go func() {
		resp, err := http.Get("http://" + srv.Addr + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()
	waitForHandler(t, entered)

	start := time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveWithDrain = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
			t.Errorf("returned after %s; the drain window was not honoured", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a permanently blocked handler hung the shutdown past its deadline")
	}
}

// TestServeReturnsAnUnexpectedListenerError pins that a listener that fails for
// a reason other than shutdown is reported, and does not deadlock waiting on a
// signal that will never arrive.
func TestServeReturnsAnUnexpectedListenerError(t *testing.T) {
	// Occupy the address so ListenAndServe fails immediately.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	srv := &http.Server{Addr: l.Addr().String(), Handler: http.NewServeMux()}
	shutdownCtx, stop := context.WithCancel(context.Background())
	defer stop()

	done := make(chan error, 1)
	go func() { done <- serveWithDrain(srv, shutdownCtx, stop, time.Second) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a listener failure was reported as a clean exit")
		}
		if errors.Is(err, http.ErrServerClosed) {
			t.Errorf("err = %v, want the listener error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveWithDrain deadlocked on a listener failure")
	}
}

// TestIdleShutdownReturnsPromptly covers the ordinary case: no requests in
// flight, so the drain has nothing to wait for.
func TestIdleShutdownReturnsPromptly(t *testing.T) {
	srv := &http.Server{Addr: freeAddr(t), Handler: http.NewServeMux()}
	shutdownCtx, stop := context.WithCancel(context.Background())
	defer stop()

	done := make(chan error, 1)
	go func() { done <- serveWithDrain(srv, shutdownCtx, stop, 10*time.Second) }()
	waitForListener(t, srv.Addr)
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveWithDrain = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle shutdown did not return")
	}
}

// TestBinaryExitsOnSIGTERM runs the REAL binary in HTTP mode and sends it a
// SIGTERM, so the wiring in main — not only the extracted helper — is covered.
func TestBinaryExitsOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "epistula-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cfgPath := writeTempConfig(t, fmt.Sprintf(`
[mailapi]
base_url = "http://127.0.0.1:8784"
token = "tok-abc"
`))
	addr := freeAddr(t)
	cmd := exec.Command(bin, "-config", cfgPath, "-http", addr)
	cmd.Env = append(os.Environ(), "MAIL_MCP_BASE_URL=", "MAIL_MCP_TOKEN=", "MAIL_MCP_HTTP_TOKEN=")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForListener(t, addr)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("the binary exited with %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the binary did not exit on SIGTERM\n%s", out.String())
	}
	if !strings.Contains(out.String(), "shutting down HTTP transport") {
		t.Errorf("the binary did not run its drain path:\n%s", out.String())
	}
}

// waitForListener blocks until something accepts on addr.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s", addr)
}

// waitForHandler blocks until the handler signals it has been entered, so the
// shutdown below races a request that is genuinely in flight rather than one
// that might not have arrived yet.
func waitForHandler(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("no request reached the handler")
	}
}
