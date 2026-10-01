package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

type pacedExportReader struct{ io.Reader }

func (r pacedExportReader) Read(p []byte) (int, error) {
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, err := r.Reader.Read(p)
	if n > 0 {
		time.Sleep(8 * time.Millisecond)
	}
	return n, err
}

func TestOneProgressingLargeExportRowOutlivesDeadline(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.cfg.Limits.ExportWriteStall = "1s"
	const size = 24 << 20
	if _, err := f.pool.Exec(context.Background(), `UPDATE messages SET text_body=repeat('x',$2) WHERE id=$1`, f.bobMsgID, size); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, size+(1<<20))
	start := time.Now()
	resp := f.do(http.MethodGet, "/v1/export?mailbox=bob", f.bobToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	reader := pacedExportReader{resp.Body}
	n := 0
	for {
		got, err := reader.Read(buffer[n:min(n+64*1024, len(buffer))])
		n += got
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("progressing row truncated after %s: %v", time.Since(start), err)
		}
		if n == len(buffer) {
			t.Fatal("unexpected export size")
		}
	}
	elapsed := time.Since(start)
	var row messageItem
	if err := json.Unmarshal(buffer[:n], &row); err != nil {
		t.Fatalf("incomplete row after %s: %v", elapsed, err)
	}
	if row.ID != f.bobMsgID || row.TextBody == nil || len(*row.TextBody) != size {
		t.Fatal("row incomplete")
	}
	if elapsed < 2*time.Second {
		t.Fatal("fixture did not outlive deadline", elapsed)
	}
	t.Logf("one complete %d-byte row progressed for %s under 1s write budget", size, elapsed)
}
