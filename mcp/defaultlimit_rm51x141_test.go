package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOmittedLimitRetriesOlderAPIOnce(t *testing.T) {
	for _, alwaysReject := range []bool{false, true} {
		t.Run(fmt.Sprint(alwaysReject), func(t *testing.T) {
			var limits []string
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				limit := r.URL.Query().Get("limit")
				limits = append(limits, limit)
				if limit == "default:200" || alwaysReject {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprint(w, `{"detail":"limit must be a positive integer"}`)
					return
				}
				writeJSON(w, map[string]any{"mailbox": "alice", "folder": "INBOX", "messages": []any{}})
			}))
			defer api.Close()
			cfg := config{BaseURL: api.URL, Token: "test-read", MaxLimit: 200, RequestTimeout: defaultRequestTimeout}
			ts := &toolset{c: newClient(cfg), cfg: cfg}
			res, _, err := ts.messages(context.Background(), nil, messagesInput{Mailbox: "alice", Folder: "INBOX", Limit: 0})
			out := decodeResult(t, res, err)
			if _, failed := out["error"]; failed != alwaysReject {
				t.Fatalf("result=%+v", out)
			}
			if !reflect.DeepEqual(limits, []string{"default:200", "200"}) {
				t.Fatalf("limits=%v", limits)
			}
		})
	}
}
