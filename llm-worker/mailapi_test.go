package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMailAPIExportAndPutAnnotation(t *testing.T) {
	var putSeen bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer mapi_test" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/export":
			if r.URL.Query().Get("mailbox") != "alice" {
				t.Fatalf("mailbox query = %q", r.URL.Query().Get("mailbox"))
			}
			if got := r.URL.Query().Get("not_annotated_by"); got != "lmstudio:m" {
				t.Fatalf("not_annotated_by query = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":1,"uid":1,"mailbox":"alice","folder":"INBOX","internal_date":"2026-06-01T00:00:00Z","flags":[],"size":10,"attachment_count":0,"text_body":"hello"}` + "\n")),
			}, nil
		case r.Method == http.MethodPut && r.URL.Path == "/v1/messages/1/annotation":
			var put AnnotationPut
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Fatalf("decode put: %v", err)
			}
			if put.Model != "m" {
				t.Fatalf("model = %q", put.Model)
			}
			putSeen = true
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})

	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: "http://mail.test", Token: "mapi_test", RequestTimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	client.client = &http.Client{Transport: rt}
	client.exportClient = client.client // Export uses exportClient; inject the same RT
	var ids []int64
	err = client.Export(context.Background(), MailAPIConfig{Mailbox: "alice"}, "lmstudio:m", func(msg Message) error {
		ids = append(ids, msg.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Export() = %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ids = %#v", ids)
	}
	if err := client.PutAnnotation(context.Background(), 1, AnnotationPut{Model: "m"}); err != nil {
		t.Fatalf("PutAnnotation() = %v", err)
	}
	if !putSeen {
		t.Fatal("PUT was not seen")
	}
}
