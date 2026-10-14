package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func exportOutcome(t *testing.T, outcome string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metricExportStreamsTotal.WithLabelValues(outcome).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestStatusRecorderStartsUnwritten(t *testing.T) {
	for _, mode := range []string{"none", "write", "flush", "header"} {
		t.Run(mode, func(t *testing.T) {
			rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
			want := 0
			switch mode {
			case "write":
				_, _ = rec.Write([]byte("ok"))
				want = 200
			case "flush":
				if err := rec.FlushError(); err != nil {
					t.Fatal(err)
				}
				want = 200
			case "header":
				rec.WriteHeader(204)
				want = 204
			}
			if rec.status != want {
				t.Fatalf("status=%d want=%d", rec.status, want)
			}
		})
	}
}

func TestAttachmentNumericOrder(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[2]
	want := []string{"1.2", "1.10", "2", "10"}
	for _, part := range want {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO attachments (message_id,part_number,content_type,size_bytes,sha256,blob_date) VALUES ($1,$2,'application/octet-stream',1,decode(repeat('ab',32),'hex'),current_date)`, id, part); err != nil {
			t.Fatal(err)
		}
	}
	var single struct {
		Attachments []attachmentItem `json:"attachments"`
	}
	f.getJSON(fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, 200, &single)
	check := func(items []attachmentItem) {
		t.Helper()
		got := make([]string, len(items))
		for i, a := range items {
			got[i] = a.PartNumber
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parts=%v want=%v", got, want)
		}
	}
	check(single.Attachments)
	before := exportOutcome(t, "completed")
	resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	found := false
	for {
		var row messageItem
		err := dec.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if row.ID == id {
			check(row.Attachments)
			found = true
		}
	}
	if !found {
		t.Fatal("message absent from export")
	}
	if got := exportOutcome(t, "completed"); got != before+1 {
		t.Fatalf("completed=%v before=%v", got, before)
	}
}
