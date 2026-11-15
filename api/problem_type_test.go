package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProblemTypeUsesProxyRelativeNamespace(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/v1/mailboxes", nil)
	recorder := httptest.NewRecorder()

	problemUnauthorized(recorder, req, "Missing bearer token.")

	var got problem
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if want := "/mail/api/errors/unauthorized"; got.Type != want {
		t.Fatalf("type = %q, want %q", got.Type, want)
	}
}
