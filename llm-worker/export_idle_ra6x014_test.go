package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// exportRow renders one NDJSON export row with a text body of n bytes.
func exportRow(id int, n int) string {
	return fmt.Sprintf(
		`{"id":%d,"uid":%d,"internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,`+
			`"attachment_count":0,"text_body":%q}`+"\n",
		id, id, strings.Repeat("a", n))
}

// newExportClient builds a client with a one-second idle budget, which is the
// smallest value the config's integer-seconds field can express.
func newExportClient(t *testing.T, url string) *MailAPIClient {
	t.Helper()
	c, err := NewMailAPIClient(MailAPIConfig{BaseURL: url, Token: "x", RequestTimeoutSec: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestExportSurvivesARowDeliveredSlowly is the RA6X-014 regression.
//
// The watchdog measured the time to obtain one COMPLETE DECODED ROW — the
// reads, the JSON decode and the two unmarshal passes. A row that takes longer
// than the budget to arrive is not an idle stream, and a healthy 33 MiB row
// tripped it: `export stream idle for 5s with no row` on a connection that had
// been delivering bytes the whole time.
func TestExportSurvivesARowDeliveredSlowly(t *testing.T) {
	row := exportRow(1, 256*1024)
	const chunks = 12
	const gap = 250 * time.Millisecond // well under the 1s budget
	// Total delivery time is 3s — three times the budget.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		per := (len(row) + chunks - 1) / chunks
		for i := 0; i < len(row); i += per {
			end := min(i+per, len(row))
			if _, err := w.Write([]byte(row[i:end])); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(gap)
		}
	}))
	defer srv.Close()

	var got []int64
	start := time.Now()
	err := newExportClient(t, srv.URL).Export(context.Background(), MailAPIConfig{}, "",
		func(m Message) error { got = append(got, m.ID); return nil })
	if err != nil {
		t.Fatalf("Export of a continuously-delivered row: %v", err)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("ids = %v, want [1]", got)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("the row arrived in %s; the test did not exercise a delivery longer than the budget", elapsed)
	}
}

// TestExportAbortsOnAStalledPartialRow pins the other side: a server that stops
// sending mid-row IS idle, and the pass must abort rather than hang.
func TestExportAbortsOnAStalledPartialRow(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(`{"id":1,"uid":1,`))
			f.Flush()
		}
		<-release // never send the rest
	}))
	defer func() { close(release); srv.Close() }()

	start := time.Now()
	err := newExportClient(t, srv.URL).Export(context.Background(), MailAPIConfig{}, "",
		func(Message) error { t.Error("a partial row reached the handler"); return nil })
	if err == nil {
		t.Fatal("a stalled partial row completed successfully")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("err = %v, want the idle abort", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the stall took %s to detect", elapsed)
	}
}

// TestExportCancelledBetweenRowsIsNotCompletion is the EOF-ordering half of
// RA6X-014. Cancelling the request closes the body, and the read that unwinds
// can surface as io.EOF — indistinguishable, at that point, from the server
// having finished. The EOF branch ran before any cancellation check, so a pass
// that was torn down reported success and the worker moved on as if the whole
// corpus had been streamed.
func TestExportCancelledBetweenRowsIsNotCompletion(t *testing.T) {
	// Two shapes, because the read may or may not be blocked when the parent
	// is cancelled: a server that keeps the connection open after the first
	// row, and one that closes immediately so EOF is already buffered.
	for _, tc := range []struct {
		name string
		hold bool
	}{
		{"connection still open", true},
		{"EOF already buffered", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(exportRow(1, 16)))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				if tc.hold {
					<-release
				}
			}))
			defer func() { close(release); srv.Close() }()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var seen int
			err := newExportClient(t, srv.URL).Export(ctx, MailAPIConfig{}, "", func(Message) error {
				seen++
				cancel() // shut down exactly at the row boundary
				return nil
			})
			if seen != 1 {
				t.Fatalf("handler ran %d times, want 1", seen)
			}
			if err == nil {
				t.Fatal("a cancelled pass reported successful completion")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// TestIdleWatchCallbackCannotOutliveItsPhase is the timer-race half of
// RA6X-014.
//
// time.Timer.Stop reports "already fired" without waiting for the callback to
// run, so a callback that had begun but not yet recorded itself could cancel a
// LATER phase while the loop, having just asked, believed nothing had fired.
// The invariant this pins is the one that makes the answer trustworthy: the
// stream is cancelled if and only if Fired reports it.
//
// Run this with -race and -count to give the window a chance; the budget is a
// microsecond precisely so enter/exit and the callback collide.
func TestIdleWatchCallbackCannotOutliveItsPhase(t *testing.T) {
	for i := 0; i < 3000; i++ {
		var cancels atomic.Int64
		w := newIdleWatch(time.Microsecond, func() { cancels.Add(1) })
		w.enter()
		w.exit()
		// Let any callback that was already running reach its mutex.
		time.Sleep(50 * time.Microsecond)
		if cancels.Load() > 0 && !w.Fired() {
			t.Fatalf("iteration %d: the stream was cancelled but Fired() says it was not", i)
		}
		if w.Fired() && cancels.Load() == 0 {
			t.Fatalf("iteration %d: Fired() reports a stall that never cancelled the stream", i)
		}
		w.Stop()
	}
}

// TestIdleWatchDoesNotCancelALaterRead pins the consequence: a watch that was
// disarmed cannot tear down the phase that follows it. A budget the second
// phase never approaches must never be charged for the first phase's stall.
func TestIdleWatchDoesNotCancelALaterRead(t *testing.T) {
	for i := 0; i < 500; i++ {
		var cancels atomic.Int64
		w := newIdleWatch(time.Microsecond, func() { cancels.Add(1) })
		w.enter()
		w.exit()
		before := cancels.Load()
		firedFirst := w.Fired()
		// A second phase that returns immediately.
		w.enter()
		w.exit()
		time.Sleep(50 * time.Microsecond)
		if !firedFirst && cancels.Load() > before && !w.Fired() {
			t.Fatalf("iteration %d: a retired callback cancelled the following read", i)
		}
		w.Stop()
	}
}
