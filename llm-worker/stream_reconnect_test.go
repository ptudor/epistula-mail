package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// cuttingStore serves the fake store's API, but its first export stream is
// cut off mid-row after two rows, the way Apache ends a long export whose
// backend connection went idle behind the proxy buffers.
type cuttingStore struct {
	*fakeStore
	mu      sync.Mutex
	exports int
	cutAt   int // rows written before the cut; 0 cuts before any
}

func (c *cuttingStore) handler(t *testing.T) http.Handler {
	api := c.mailAPI(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/export" {
			api.ServeHTTP(w, r)
			return
		}
		c.mu.Lock()
		c.exports++
		first := c.exports == 1
		c.mu.Unlock()
		if !first {
			api.ServeHTTP(w, r)
			return
		}
		// Write cutAt complete rows and half of the next, then drop the
		// connection.
		c.fakeStore.mu.Lock()
		var rows []string
		for id := int64(1); id <= int64(len(c.messages)); id++ {
			if c.annotated[id] {
				continue
			}
			b, _ := json.Marshal(c.messages[id])
			rows = append(rows, string(b))
		}
		c.fakeStore.mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Join(rows[:c.cutAt], "\n")
		if c.cutAt > 0 {
			body += "\n"
		}
		body += rows[c.cutAt][:len(rows[c.cutAt])/2]
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n")
		buf.WriteString(strconv.FormatInt(int64(len(body)), 16) + "\r\n" + body + "\r\n")
		buf.Flush()
		conn.Close()
	})
}

func newCuttingWorker(t *testing.T, c *cuttingStore) *Worker {
	t.Helper()
	api := httptest.NewServer(c.handler(t))
	t.Cleanup(api.Close)
	lm := httptest.NewServer(c.lmStudio(t))
	t.Cleanup(lm.Close)
	cfg := DefaultConfig()
	cfg.MailAPI.BaseURL = api.URL
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = lm.URL + "/v1"
	cfg.Worker.AnnotationModel = "lmstudio:test"
	cfg.Worker.RetryAttempts = 0
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestCutExportStreamIsReopened: a stream cut after delivering rows is
// reopened within the same pass, which then completes with every message
// annotated once.
func TestCutExportStreamIsReopened(t *testing.T) {
	c := &cuttingStore{fakeStore: newFakeStore(), cutAt: 2}
	w := newCuttingWorker(t, c)
	before := metricStreamReconnects.Load()
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v; want the cut stream reopened", err)
	}
	if c.exports != 2 || metricStreamReconnects.Load()-before != 1 {
		t.Fatalf("exports = %d, reconnects = %d; want 2 and 1", c.exports, metricStreamReconnects.Load()-before)
	}
	if stats.Annotated != 3 {
		t.Fatalf("annotated %d, want the 3 unannotated messages once each", stats.Annotated)
	}
}

// TestCutExportStreamStandsWhenNotResumable: a stream cut before any row, or
// a pass without a filter that excludes finished work, keeps its error.
func TestCutExportStreamStandsWhenNotResumable(t *testing.T) {
	c := &cuttingStore{fakeStore: newFakeStore(), cutAt: 0}
	w := newCuttingWorker(t, c)
	if _, err := w.RunOnce(context.Background()); err == nil || c.exports != 1 {
		t.Fatalf("cut before any row: err = %v after %d export(s); want the error, no reopen", err, c.exports)
	}

	c = &cuttingStore{fakeStore: newFakeStore(), cutAt: 2}
	w = newCuttingWorker(t, c)
	w.cfg.Worker.SkipAnnotated = false // no not_annotated_by: a reopen would start over
	if _, err := w.RunOnce(context.Background()); err == nil || c.exports != 1 {
		t.Fatalf("unfiltered pass: err = %v after %d export(s); want the error, no reopen", err, c.exports)
	}
}
