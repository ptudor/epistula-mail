package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNegotiatedDefaultThroughListAndSearch(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.cfg.Limits.DefaultPageSize = 7
	f.srv.cfg.Limits.MaxPageSize = 100
	for _, prefix := range []string{"/v1/mailboxes/alice/folders/INBOX/messages?", "/v1/search?q=gophers&"} {
		for _, tc := range []struct {
			limit string
			want  int
		}{{"default:100", 7}, {"default:3", 3}, {"2", 2}} {
			var out struct{ Messages []messageItem }
			f.getJSON(prefix+"limit="+tc.limit, f.aliceContentToken, http.StatusOK, &out)
			if len(out.Messages) != tc.want {
				t.Fatalf("%s %s: %d, want %d", prefix, tc.limit, len(out.Messages), tc.want)
			}
		}
	}
}

func TestNegotiatedDefaultLimit(t *testing.T) {
	for _, def := range []int{7, 50, 150} {
		s := &server{cfg: DefaultConfig()}
		s.cfg.Limits.DefaultPageSize = def
		s.cfg.Limits.MaxPageSize = 200
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"", def}, {"?limit=default:10", min(def, 10)}, {"?limit=default:100", min(def, 100)},
			{"?limit=10", 10}, {"?limit=999", 200}, {"?limit=default:0", 0}, {"?limit=default:-1", 0},
			{"?limit=default:999999999999999999999", 0}, {"?limit=default:x", 0},
		} {
			got, err := s.pageSize(httptest.NewRequest("GET", "/v1/search"+tc.query, nil))
			if tc.want == 0 {
				if err == nil {
					t.Fatal("invalid cap accepted", tc.query)
				}
				continue
			}
			if err != nil || got != tc.want {
				t.Fatalf("default=%d query=%s: %d %v, want %d", def, tc.query, got, err, tc.want)
			}
		}
	}
}
