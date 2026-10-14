package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDateAndUIDCursorRanges(t *testing.T) {
	f := newAPIFixture(t)
	for _, cursor := range []string{
		encodeCursor(math.MaxInt64, 1), encodeCursor(math.MinInt64, -1),
		encodeCursor(-1, 1), encodeCursor(maxDateCursorMicros+1, 1), encodeCursor(0, -1),
	} {
		p := wantProblem(t, f.do(http.MethodGet, "/v1/search?q=x&cursor="+cursor, f.classifierToken, nil), http.StatusUnprocessableEntity)
		if p.Detail != "Malformed cursor." {
			t.Fatal(p.Detail)
		}
	}
	wantProblem(t, f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages?cursor="+encodeCursor(-1), f.classifierToken, nil), http.StatusUnprocessableEntity)
	for _, micros := range []int64{0, maxDateCursorMicros} {
		resp := f.do(http.MethodGet, "/v1/search?q=x&cursor="+encodeCursor(micros, 0), f.classifierToken, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
	}
}

func TestUncleanPathsAuthenticateWithoutRedirect(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{
		"/v1/mailboxes/alice/folders/A/../B/messages",
		"/v1//mailboxes/alice/folders/INBOX/messages",
	} {
		for _, token := range []string{"", f.classifierToken} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			want := http.StatusUnauthorized
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
				want = http.StatusNotFound
			}
			w := httptest.NewRecorder()
			f.srv.routes().ServeHTTP(w, r)
			if w.Code != want || w.Header().Get("Location") != "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("unclean route = %d, %v, %s", w.Code, w.Header(), w.Body.String())
			}
		}
	}
}
