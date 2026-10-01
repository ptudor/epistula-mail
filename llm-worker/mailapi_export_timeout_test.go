package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ndjsonRow(id int64) string {
	return fmt.Sprintf(`{"id":%d,"uid":%d,"internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,"attachment_count":0}`+"\n", id, id)
}

// TestExportCompletesDespiteSlowHandler is the R-009 regression: the worker
// does per-message work between reads, and the *cumulative* time can exceed
// request_timeout_seconds. Under the old shared http.Client.Timeout (a total
// deadline) the pass aborted mid-stream; with the dedicated Timeout:0 export
// client plus the per-row idle watchdog, all rows complete as long as each
// gap stays under the budget.
func TestExportCompletesDespiteSlowHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := int64(1); i <= 3; i++ {
			io.WriteString(w, ndjsonRow(i))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, Token: "x", RequestTimeoutSec: 1})
	if err != nil {
		t.Fatal(err)
	}

	var ids []int64
	err = client.Export(context.Background(), MailAPIConfig{}, "", func(m Message) error {
		// 600ms per message: each gap < 1s budget, but total 1.8s > the old
		// 1s total http.Client.Timeout that used to kill the stream.
		time.Sleep(600 * time.Millisecond)
		ids = append(ids, m.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Export: %v (all rows should complete despite slow handler)", err)
	}
	if len(ids) != 3 {
		t.Fatalf("processed %d rows, want 3 (%v)", len(ids), ids)
	}
}

// TestExportAbortsOnIdleStream is the second R-009 case: a server that sends
// one row then hangs must abort after the idle window, not hang forever.
func TestExportAbortsOnIdleStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			io.WriteString(w, ndjsonRow(1))
			fl.Flush()
		}
		<-release // hang until the test releases us
	}))
	defer srv.Close()
	defer close(release)

	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, Token: "x", RequestTimeoutSec: 1})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var ids []int64
	err = client.Export(context.Background(), MailAPIConfig{}, "", func(m Message) error {
		ids = append(ids, m.ID)
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Export returned nil; expected an idle-timeout abort")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("Export err = %q, want an idle-timeout error", err)
	}
	if len(ids) != 1 {
		t.Fatalf("processed %d rows, want 1 before the hang", len(ids))
	}
	if elapsed > 5*time.Second {
		t.Fatalf("aborted after %v; the ~1s watchdog should have fired much sooner", elapsed)
	}
}

// TestExportSurvivesHandlerSlowerThanTimeout is the RO5X-015 regression.
//
// The watchdog used to be Reset *before* handle(), so one requestTimeout
// budget had to cover [LLM inference + retries + backoff + the next decode].
// With the shipped defaults that is 3 x 300s + 2 x 2s = 904s against a 300s
// watchdog, so a healthy pass died with "export stream idle …" — blaming
// epistula-api for the worker's own latency, and skipping every remaining message.
//
// The second row must arrive from the network AFTER the slow handler returns,
// or the decoder would satisfy it from its buffer and never exercise the
// watchdog at all. A channel sequences the server against the handler so the
// race is deterministic rather than timing-dependent.
func TestExportSurvivesHandlerSlowerThanTimeout(t *testing.T) {
	firstHandled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		io.WriteString(w, ndjsonRow(1))
		if fl != nil {
			fl.Flush()
		}
		// Hold row 2 back until the slow handler has finished row 1.
		<-firstHandled
		io.WriteString(w, ndjsonRow(2))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, Token: "x", RequestTimeoutSec: 1})
	if err != nil {
		t.Fatal(err)
	}

	var ids []int64
	err = client.Export(context.Background(), MailAPIConfig{}, "", func(m Message) error {
		ids = append(ids, m.ID)
		if m.ID == 1 {
			// 2x the 1s watchdog budget: one slow inference.
			time.Sleep(2 * time.Second)
			close(firstHandled)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Export: %v — per-message work must not count as stream idle time (RO5X-015)", err)
	}
	if len(ids) != 2 {
		t.Fatalf("processed %v, want both rows", ids)
	}
}

// TestExportStillAbortsOnIdleAfterSlowHandler proves the watchdog is genuinely
// re-armed after handle() returns: a slow message followed by a server that
// goes silent must still trip the idle abort rather than hanging forever.
func TestExportStillAbortsOnIdleAfterSlowHandler(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			io.WriteString(w, ndjsonRow(1))
			fl.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, Token: "x", RequestTimeoutSec: 1})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = client.Export(context.Background(), MailAPIConfig{}, "", func(m Message) error {
		time.Sleep(1500 * time.Millisecond) // longer than the watchdog budget
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Export returned nil; the idle watchdog must still fire after a slow handler")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("Export err = %q, want an idle-timeout error", err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("aborted after %v; the watchdog should re-arm promptly after handle()", elapsed)
	}
}
