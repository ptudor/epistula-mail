package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/auth"
)

func TestNamedMailboxScopeDoesNotRevealExistence(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.classificationsAvailable = true
	f.srv.passQueueAvailable = true
	scoped := f.mintToken(context.Background(), "alice-annotator", []string{"alice"}, []string{auth.PermissionReadContent, auth.PermissionWriteAnnotation})
	for _, route := range []string{
		"/v1/mailboxes/%s/folders",
		"/v1/mailboxes/%s/folders/INBOX/messages",
		"/v1/search?q=x&mailbox=%s",
		"/v1/export?mailbox=%s",
		"/v1/mailboxes/%s/archive-categories",
		"/v1/pass-required/prune",
	} {
		t.Run(route, func(t *testing.T) {
			for _, name := range []string{"bob", "zzz-absent"} {
				method, url, body := http.MethodGet, fmt.Sprintf(route, name), ""
				if route == "/v1/pass-required/prune" {
					method, url, body = http.MethodPost, route, fmt.Sprintf(`{"model":"m1","mailbox":%q}`, name)
				}
				p := wantProblem(t, f.do(method, url, scoped, strings.NewReader(body)), http.StatusForbidden)
				if p.Detail != "Token is not authorized for this mailbox." {
					t.Fatalf("name %q exposed different detail: %q", name, p.Detail)
				}
				if name == "zzz-absent" {
					wantProblem(t, f.do(method, url, f.classifierToken, strings.NewReader(body)), http.StatusNotFound)
				}
			}
		})
	}
	wantProblem(t, f.do(http.MethodGet, "/v1/mailboxes/alice/folders/absent/messages", scoped, nil), http.StatusNotFound)
}
