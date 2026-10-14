package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthFailureDelayReleasesArgonSlot(t *testing.T) {
	f := newAPIFixture(t)
	a := f.srv.auth
	a.argonSem = make(chan struct{}, 1)
	f.srv.cfg.Limits.AuthFailDelay = "500ms"
	parts := strings.SplitN(f.classifierToken, "_", 3)
	parts[2] = strings.Repeat("a", len(parts[2]))
	bad := strings.Join(parts, "_")
	request := func(ip string) <-chan int {
		done := make(chan int, 1)
		go func() {
			r := httptest.NewRequest(http.MethodGet, "/v1/mailboxes", nil)
			r.RemoteAddr = ip + ":1234"
			r.Header.Set("Authorization", "Bearer "+bad)
			w := httptest.NewRecorder()
			a.authenticate(w, r)
			done <- w.Code
		}()
		return done
	}
	waitFailure := func(ip string) {
		t.Helper()
		deadline := time.Now().Add(400 * time.Millisecond)
		for time.Now().Before(deadline) {
			a.mu.Lock()
			n := len(a.fails[ip])
			a.mu.Unlock()
			if n != 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("request %s never entered its failure delay", ip)
	}
	first := request("192.0.2.1")
	waitFailure("192.0.2.1")
	if len(a.argonSem) != 0 {
		t.Fatal("failure delay holds the Argon slot")
	}
	second := request("192.0.2.2")
	waitFailure("192.0.2.2")
	select {
	case <-first:
		t.Fatal("first request finished before the second reached its delay")
	default:
	}
	for _, done := range []<-chan int{first, second} {
		select {
		case status := <-done:
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("authentication did not finish")
		}
	}
}

func TestCachedTokenBypassesIPFailureThrottle(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/mailboxes", f.classifierToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	for range f.srv.cfg.Limits.AuthFailLimit {
		f.srv.auth.recordFailure("127.0.0.1")
	}
	resp = f.do(http.MethodGet, "/v1/mailboxes", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached token: %s", resp.Status)
	}
}

func TestClientIPIgnoresUntrustedLoopback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	if got := clientIP(r, nil); got != "127.0.0.1" {
		t.Fatal(got)
	}
}

func TestTrustedProxiesValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Server.TrustedProxies = []string{"127.0.0.1", "::1/128"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Server.TrustedProxies = []string{"localhost"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("validation: %v", err)
	}
}
