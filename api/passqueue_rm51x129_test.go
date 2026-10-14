package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestMissingPassQueueHasSpecificProblemType(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{"/v1/export?pass_required=true", "/v1/pass-required/prune"} {
		method, body := http.MethodGet, ""
		if strings.Contains(path, "prune") {
			method, body = http.MethodPost, `{"model":"m1"}`
		}
		p := wantProblem(t, f.do(method, path, f.classifierToken, strings.NewReader(body)), http.StatusServiceUnavailable)
		if p.Type != problemTypeBase+"unavailable-pass-queue" {
			t.Fatalf("type = %s", p.Type)
		}
	}
}
