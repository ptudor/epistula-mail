package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFourBytePreviewAllocationBound(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]
	if _, err := f.pool.Exec(context.Background(), `UPDATE messages SET text_body=repeat('😀',1048576) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/v1/messages/%d?html=false&text_limit=65536", id)
	// Warm HTTP, authentication, statement and scan caches before measuring.
	for range 2 {
		resp := f.do(http.MethodGet, url, f.aliceContentToken, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp := f.do(http.MethodGet, url, f.aliceContentToken, nil)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	runtime.ReadMemStats(&after)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%s: %v", body, err)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 2<<20 {
		t.Fatalf("preview allocated %d bytes", delta)
	} else {
		t.Logf("65536-byte emoji preview allocated %d bytes", delta)
	}
	for _, limit := range []int{1, 3, 65535, 65536} {
		var doc messageDoc
		f.getJSON(fmt.Sprintf("/v1/messages/%d?html=false&text_limit=%d", id, limit), f.aliceContentToken, http.StatusOK, &doc)
		want := limit / 4 * 4
		if want == 0 {
			want = 4
		}
		if doc.TextBody == nil || !utf8.ValidString(*doc.TextBody) || len(*doc.TextBody) != want || *doc.TextBody != strings.Repeat("😀", want/4) {
			t.Fatalf("limit=%d invalid preview", limit)
		}
	}
}
