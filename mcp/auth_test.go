package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerAuth(t *testing.T) {
	const token = "s3cr3t-token"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := bearerAuth(token, next)

	cases := []struct {
		name       string
		authHeader string
		want       int
	}{
		{"valid", "Bearer " + token, http.StatusOK},
		{"case-insensitive scheme", "bearer " + token, http.StatusOK},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"missing header", "", http.StatusUnauthorized},
		{"no scheme", token, http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("missing WWW-Authenticate: Bearer on 401")
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8786":   true,
		"localhost:8786":   true,
		"[::1]:8786":       true,
		"127.0.0.5:8786":   true,
		":8786":            false, // wildcard binds every interface
		"0.0.0.0:8786":     false,
		"[::]:8786":        false,
		"192.168.1.10:878": false,
		"10.0.0.1:8786":    false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
