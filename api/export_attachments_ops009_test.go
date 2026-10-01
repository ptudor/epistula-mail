package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestExportCarriesAttachmentMetadata is OPS-009. The annotation worker reads
// /v1/export and saw only attachment_count, so an unsolicited "payment
// receipt" whose one attachment was RECIBO DE PAGO.rar was filed as a genuine
// receipt. Each export row now lists its attachments as GET /v1/messages/{id}
// does, in part order; a message with none has no attachments field.
func TestExportCarriesAttachmentMetadata(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	withAtt := f.aliceMsgIDs[2]
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO attachments (message_id, part_number, filename, content_type, disposition, size_bytes, sha256, blob_date)
		VALUES ($1, '3', 'RECIBO DE PAGO.rar', 'application/x-rar-compressed', 'attachment', 791294, decode(repeat('ab',32),'hex'), current_date),
		       ($1, '2', NULL, 'image/png', 'inline', 2402, decode(repeat('cd',32),'hex'), current_date)`,
		withAtt); err != nil {
		t.Fatal(err)
	}

	resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status %d", resp.StatusCode)
	}
	rows := map[int64]map[string]json.RawMessage{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("row %q: %v", sc.Text(), err)
		}
		id, err := strconv.ParseInt(string(row["id"]), 10, 64)
		if err != nil {
			t.Fatalf("row id %s: %v", row["id"], err)
		}
		rows[id] = row
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(f.aliceMsgIDs) {
		t.Fatalf("export returned %d rows, want %d", len(rows), len(f.aliceMsgIDs))
	}

	var atts []attachmentItem
	if err := json.Unmarshal(rows[withAtt]["attachments"], &atts); err != nil {
		t.Fatalf("attachments of %d: %v (%s)", withAtt, err, rows[withAtt]["attachments"])
	}
	if len(atts) != 2 {
		t.Fatalf("attachments = %+v, want 2", atts)
	}
	if atts[0].PartNumber != "2" || atts[0].Filename != nil || atts[0].ContentType != "image/png" || atts[0].SizeBytes != 2402 {
		t.Errorf("first attachment = %+v, want the unnamed image/png at part 2", atts[0])
	}
	if atts[1].PartNumber != "3" || atts[1].Filename == nil || *atts[1].Filename != "RECIBO DE PAGO.rar" ||
		atts[1].ContentType != "application/x-rar-compressed" || atts[1].SizeBytes != 791294 {
		t.Errorf("second attachment = %+v, want RECIBO DE PAGO.rar at part 3", atts[1])
	}
	for id, row := range rows {
		if _, ok := row["attachments"]; ok && id != withAtt {
			t.Errorf("message %d has no attachments but its row carries the field: %s", id, row["attachments"])
		}
	}

	// The list endpoint does not grow the field.
	var page struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?limit=10", f.classifierToken, http.StatusOK, &page)
	for _, m := range page.Messages {
		if _, ok := m["attachments"]; ok {
			t.Errorf("list row %s carries attachments", m["id"])
		}
	}
}
