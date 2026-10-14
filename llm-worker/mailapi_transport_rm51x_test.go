package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExportTransportPreservesDefaults(t *testing.T) {
	c, err := NewMailAPIClient(MailAPIConfig{BaseURL: "http://127.0.0.1", RequestTimeoutSec: 7})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.exportClient.Transport.(*http.Transport)
	defaults := http.DefaultTransport.(*http.Transport)
	if tr == defaults || tr.Proxy == nil || tr.IdleConnTimeout != defaults.IdleConnTimeout || tr.MaxIdleConns != defaults.MaxIdleConns || tr.ForceAttemptHTTP2 != defaults.ForceAttemptHTTP2 || tr.ResponseHeaderTimeout != 7*time.Second || c.exportClient.Timeout != 0 {
		t.Fatalf("export transport=%+v", tr)
	}
}

func TestArchiveCategoriesEscapesMailboxOnce(t *testing.T) {
	name := "name% with space"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/mailboxes/"+name+"/archive-categories"; got != want {
			t.Errorf("path=%q want=%q", got, want)
		}
		fmt.Fprint(w, `{"categories":[]}`)
	}))
	defer srv.Close()
	c, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, RequestTimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ArchiveCategories(context.Background(), name); err != nil {
		t.Fatal(err)
	}
}
