package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"
)

func TestInspectionRankingAndSummaryEdges(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	id := f.aliceMsgIDs[0]
	f.srv.modelPriorityAvailable = true
	if _, err := f.pool.Exec(ctx, `INSERT INTO message_annotations(message_id,model,tags,summary) VALUES($1,'a',ARRAY[]::text[],'abé€😀end'),($1,'z',ARRAY[]::text[],'preferred')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO annotation_models(model,priority) VALUES('z',10)`); err != nil {
		t.Fatal(err)
	}
	var doc messageDoc
	f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection", id), f.aliceContentToken, http.StatusOK, &doc)
	if len(doc.Annotations) != 2 || doc.Annotations[0].Model != "a" || doc.Annotations[0].Primary || !doc.Annotations[1].Primary {
		t.Fatal("ranked model selection changed")
	}
	for _, offset := range []int{0, 3, 6, 9, 100} {
		var page messageDoc
		f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection&annotation_model=a&summary_offset=%d", id, offset), f.aliceContentToken, http.StatusOK, &page)
		a := page.Annotations[0]
		start, body := sliceTextBody("abé€😀end", offset, 16384)
		if a.SummaryOffset == nil || *a.SummaryOffset != int64(start) || *a.Summary != body {
			t.Fatal("summary byte boundary", offset, a)
		}
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO annotation_models(model,priority,retired_at) VALUES('a',0,now()) ON CONFLICT(model) DO UPDATE SET retired_at=now(); UPDATE annotation_models SET retired_at=now()`); err != nil {
		t.Fatal(err)
	}
	doc = messageDoc{}
	f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection", id), f.aliceContentToken, http.StatusOK, &doc)
	for _, a := range doc.Annotations {
		if a.Primary {
			t.Fatal("retired model became primary")
		}
	}
	for _, q := range []string{"summary_offset=-1", "summary_offset=1", "summary_offset=nope", "summary_offset=999999999999999999999"} {
		resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d?metadata=inspection&%s", id, q), f.aliceContentToken, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatal(q, resp.StatusCode)
		}
	}
}

func TestInspectionBoundsLargeBodiesHeadersAndSidecars(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	id := f.aliceMsgIDs[0]
	// Generate large values in PostgreSQL so the allocation measurement does
	// not include the fixture. Each old eager field alone exceeds MCP's cap.
	if _, err := f.pool.Exec(ctx, `UPDATE messages SET text_body=repeat('é',6*1024*1024),html_body=repeat('h',12*1024*1024),
 headers=jsonb_build_object('X-Large',repeat('x',12*1024*1024)),bodystructure=jsonb_build_object('large',repeat('x',12*1024*1024)) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO message_annotations(message_id,model,tags,summary)
 SELECT $1,'model-'||lpad(n::text,3,'0'),ARRAY['fixture'],repeat('é',100000) FROM generate_series(1,41) n`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO attachments(message_id,part_number,filename,content_type,size_bytes,sha256,blob_date)
 SELECT $1,'part-'||lpad(n::text,3,'0'),'file-'||n,'application/octet-stream',1,decode(repeat('ab',32),'hex'),current_date FROM generate_series(1,11) n`, id); err != nil {
		t.Fatal(err)
	}
	// Warm authentication outside the allocation measurement.
	var warm any
	f.getJSON("/v1/mailboxes", f.aliceContentToken, http.StatusOK, &warm)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d?metadata=inspection&text_limit=65536", id), f.aliceContentToken, nil)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if len(data) > 1<<20 {
		t.Fatal("inspection response too large", len(data))
	}
	// Old SELECT transferred over 48 MiB before the Go-side preview ran.
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 8<<20 {
		t.Fatalf("inspection allocated %d bytes", delta)
	} else {
		t.Logf("inspection response=%d bytes, allocated=%d", len(data), delta)
	}
	var first messageDoc
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if !first.TextTruncated || first.TextBytes == nil || *first.TextBytes != 12<<20 || len(*first.TextBody) != 65536 || first.HTMLBody != nil {
		t.Fatal("preview metadata incorrect")
	}
	if len(first.Annotations) != 4 || len(first.Attachments) != 4 || first.AttachmentCount != 11 {
		t.Fatalf("first metadata page: %+v", first)
	}
	models, parts := map[string]bool{}, map[string]bool{}
	doc := first
	for {
		for _, a := range doc.Annotations {
			if models[a.Model] {
				t.Fatal("duplicate model", a.Model)
			}
			models[a.Model] = true
		}
		if doc.NextAnnotation == "" {
			break
		}
		var next messageDoc
		f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection&text_limit=4&annotation_after=%s", id, url.QueryEscape(doc.NextAnnotation)), f.aliceContentToken, http.StatusOK, &next)
		doc = next
	}
	doc = first
	for {
		for _, a := range doc.Attachments {
			if parts[a.PartNumber] {
				t.Fatal("duplicate part", a.PartNumber)
			}
			parts[a.PartNumber] = true
		}
		if doc.NextAttachment == "" {
			break
		}
		var next messageDoc
		f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection&text_limit=4&attachment_after=%s", id, url.QueryEscape(doc.NextAttachment)), f.aliceContentToken, http.StatusOK, &next)
		doc = next
	}
	if len(models) != 41 || len(parts) != 11 {
		t.Fatalf("lost metadata: %d models %d parts", len(models), len(parts))
	}
	var summary strings.Builder
	a := first.Annotations[0]
	for {
		if a.Summary == nil {
			t.Fatal("missing summary")
		}
		summary.WriteString(*a.Summary)
		if a.SummaryNextOffset == nil {
			break
		}
		var next messageDoc
		f.getJSON(fmt.Sprintf("/v1/messages/%d?metadata=inspection&text_limit=4&annotation_model=%s&summary_offset=%d", id, url.QueryEscape(a.Model), *a.SummaryNextOffset), f.aliceContentToken, http.StatusOK, &next)
		if len(next.Annotations) != 1 {
			t.Fatal("summary model changed")
		}
		a = next.Annotations[0]
	}
	if summary.String() != strings.Repeat("é", 100000) {
		t.Fatal("summary pagination lost bytes")
	}
	// The separate body tool must return a bounded byte window even near EOF.
	resp = f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/text?offset=%d&limit=65536", id, (12<<20)-65536), f.aliceContentToken, nil)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != strings.Repeat("é", 32768) {
		t.Fatal("large paged text", resp.StatusCode, err, len(body))
	}
}
