package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mockAttachmentLimit is the max_attachment_bytes newTestToolset configures.
const mockAttachmentLimit = 4096

var (
	mockPDF  = []byte("%PDF-1.4\n% Digi-Key invoice fixture\n%%EOF\n")
	mockPNG  = []byte("\x89PNG\r\n\x1a\nimage bytes")
	mockCSV  = "part,qty\nLM317,2\n"
	mockTIFF = []byte("II*\x00tiff bytes")
	// mockLatin1 is text/plain that is not UTF-8, as a legacy mailer sends it.
	mockLatin1 = []byte("Gr\xf6\xdfe")
)

// serveMockAttachment answers GET /v1/messages/{id}/attachments/{part} the way
// epistula-api does: the file with its type, a Content-Disposition carrying
// the filename, and a declared length.
func serveMockAttachment(w http.ResponseWriter, part string) {
	file := func(contentType, filename string, data []byte) {
		w.Header().Set("Content-Type", contentType)
		disposition := "attachment"
		if filename != "" {
			disposition = mime.FormatMediaType("attachment", map[string]string{"filename": filename})
		}
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}
	switch part {
	case "2":
		file("application/pdf", "Digi-Key Invoice 2014.pdf", mockPDF)
	case "3":
		file("image/png", "", mockPNG)
	case "4":
		file("text/csv; charset=utf-8", "bom.csv", []byte(mockCSV))
	case "5":
		file("image/tiff", "scan.tif", mockTIFF)
	case "6":
		file("text/plain", "legacy.txt", mockLatin1)
	case "7":
		// Over the limit, and says so up front.
		file("application/pdf", "huge.pdf", bytes.Repeat([]byte("x"), mockAttachmentLimit+1))
	case "8":
		// Over the limit with no declared length: flushing before the
		// handler returns sends the body chunked.
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(bytes.Repeat([]byte("x"), mockAttachmentLimit))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("x"))
	case "9":
		file("application/pdf", "empty.pdf", nil)
	case "1.2":
		file("application/octet-stream", "", []byte("nested"))
	default:
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "https://api.ptudor.invalid/errors/not-found", "title": "Not Found",
			"status": 404, "detail": "No such attachment.",
		})
	}
}

func callAttachment(t *testing.T, ts *toolset, part string) *mcp.CallToolResult {
	t.Helper()
	res, _, err := ts.attachment(context.Background(), nil, attachmentInput{ID: 42, PartNumber: part})
	if err != nil {
		t.Fatalf("attachment %s returned a protocol error: %v", part, err)
	}
	return res
}

func TestAttachmentPDFIsEmbeddedBlob(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res := callAttachment(t, ts, "2")

	if m.lastPath != "/v1/messages/42/attachments/2" {
		t.Errorf("upstream path = %q", m.lastPath)
	}
	// The read token, never the annotate token: fetching content is a read.
	if m.lastAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the read token", m.lastAuth)
	}
	info := decodeResult(t, res, nil)
	want := map[string]any{
		"id": float64(42), "part_number": "2", "content_type": "application/pdf",
		"size_bytes": float64(len(mockPDF)), "filename": "Digi-Key Invoice 2014.pdf",
		"returned_as": "binary resource",
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("info[%q] = %v, want %v", k, info[k], v)
		}
	}
	if len(res.Content) != 2 {
		t.Fatalf("content blocks = %d, want text + file", len(res.Content))
	}
	er, ok := res.Content[1].(*mcp.EmbeddedResource)
	if !ok {
		t.Fatalf("file block is %T, want *mcp.EmbeddedResource", res.Content[1])
	}
	if er.Resource.MIMEType != "application/pdf" || !bytes.Equal(er.Resource.Blob, mockPDF) || er.Resource.Text != "" {
		t.Errorf("resource = %+v, want the PDF bytes as a blob", er.Resource)
	}
	if er.Resource.URI != "epistula:///messages/42/attachments/2" {
		t.Errorf("resource URI = %q", er.Resource.URI)
	}
}

func TestAttachmentContentTypes(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)

	t.Run("model image is an image block", func(t *testing.T) {
		res := callAttachment(t, ts, "3")
		if len(res.Content) != 2 {
			t.Fatalf("content blocks = %d, want 2", len(res.Content))
		}
		img, ok := res.Content[1].(*mcp.ImageContent)
		if !ok || img.MIMEType != "image/png" || !bytes.Equal(img.Data, mockPNG) {
			t.Fatalf("file block = %#v, want the PNG as an image", res.Content[1])
		}
		if info := decodeResult(t, res, nil); info["returned_as"] != "image" {
			t.Errorf("returned_as = %v, want image", info["returned_as"])
		}
		if _, ok := decodeResult(t, res, nil)["filename"]; ok {
			t.Error("an unnamed attachment reported a filename")
		}
	})

	t.Run("UTF-8 text is a text resource", func(t *testing.T) {
		res := callAttachment(t, ts, "4")
		er, ok := res.Content[1].(*mcp.EmbeddedResource)
		if !ok || er.Resource.Text != mockCSV || er.Resource.Blob != nil {
			t.Fatalf("file block = %#v, want the CSV as text", res.Content[1])
		}
		// The charset parameter is dropped: the text is UTF-8 by construction.
		if er.Resource.MIMEType != "text/csv" {
			t.Errorf("MIME type = %q, want text/csv", er.Resource.MIMEType)
		}
	})

	for _, tc := range []struct{ part, mime string }{
		{"5", "image/tiff"}, // an image no model accepts as an image block
		{"6", "text/plain"}, // text that is not UTF-8
	} {
		t.Run(tc.mime+" is a blob", func(t *testing.T) {
			res := callAttachment(t, ts, tc.part)
			er, ok := res.Content[1].(*mcp.EmbeddedResource)
			if !ok || er.Resource.MIMEType != tc.mime || len(er.Resource.Blob) == 0 || er.Resource.Text != "" {
				t.Fatalf("file block = %#v, want a %s blob", res.Content[1], tc.mime)
			}
		})
	}

	t.Run("empty file has no file block", func(t *testing.T) {
		res := callAttachment(t, ts, "9")
		if len(res.Content) != 1 {
			t.Fatalf("content blocks = %d, want only the description", len(res.Content))
		}
		info := decodeResult(t, res, nil)
		if info["size_bytes"] != float64(0) || info["returned_as"] != nil {
			t.Errorf("info = %v, want size 0 and nothing returned", info)
		}
	})
}

// TestAttachmentNestedPartIsOnePathSegment: a dotted part number travels as
// data in one segment, escaped like every other name this connector puts in a
// path (RA6X-057), and arrives intact.
func TestAttachmentNestedPartIsOnePathSegment(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res := callAttachment(t, ts, "1.2")
	if m.lastPath != "/v1/messages/42/attachments/1.2" || m.lastRawPath != "/v1/messages/42/attachments/1%2E2" {
		t.Errorf("upstream path = %q (raw %q)", m.lastPath, m.lastRawPath)
	}
	if info := decodeResult(t, res, nil); info["part_number"] != "1.2" {
		t.Errorf("part_number = %v", info["part_number"])
	}
}

func TestAttachmentOverLimitIsRefused(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	for _, tc := range []struct {
		part string
		// declared is set when the refusal must come from Content-Length,
		// before any of the body is read.
		declared bool
	}{{"7", true}, {"8", false}} {
		part := tc.part
		res := callAttachment(t, ts, part)
		if len(res.Content) != 1 {
			t.Errorf("part %s: %d content blocks, want only the error", part, len(res.Content))
		}
		e, ok := decodeResult(t, res, nil)["error"].(map[string]any)
		if !ok {
			t.Fatalf("part %s: want an {\"error\": …} result", part)
		}
		msg, _ := e["message"].(string)
		if !strings.Contains(msg, "max_attachment_bytes") || !strings.Contains(msg, strconv.Itoa(mockAttachmentLimit)) {
			t.Errorf("part %s: error = %q, want the max_attachment_bytes limit named", part, msg)
		}
		declaredSize := "is " + strconv.Itoa(mockAttachmentLimit+1) + " bytes"
		if strings.Contains(msg, declaredSize) != tc.declared {
			t.Errorf("part %s: error = %q; declared-length refusal = %v, want %v",
				part, msg, !tc.declared, tc.declared)
		}
	}
}

func TestAttachmentNotFoundIsCorrectableError(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	e, ok := decodeResult(t, callAttachment(t, ts, "99"), nil)["error"].(map[string]any)
	if !ok {
		t.Fatal("want an {\"error\": …} result")
	}
	if e["status"] != float64(404) || e["detail"] != "No such attachment." {
		t.Errorf("error = %v, want the upstream 404 problem", e)
	}
}

func TestAttachmentArgumentErrors(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	for _, in := range []attachmentInput{
		{ID: 0, PartNumber: "2"},
		{ID: -1, PartNumber: "2"},
		{ID: 42, PartNumber: ""},
		{ID: 42, PartNumber: "  "},
	} {
		res, _, err := ts.attachment(context.Background(), nil, in)
		if _, ok := decodeResult(t, res, err)["error"]; !ok {
			t.Errorf("%+v: want an argument error", in)
		}
	}
	if m.lastPath != "" {
		t.Errorf("an invalid argument still reached epistula-api: %q", m.lastPath)
	}
}

// TestAttachmentOverMCPWire runs the tool through a real MCP session, so the
// file survives the protocol's JSON (base64) encoding and decodes on the
// client side as the content type the tool built.
func TestAttachmentOverMCPWire(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "mail", Version: "test"}, nil)
	ts.register(server)
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	call := func(part string) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "attachment",
			Arguments: map[string]any{"id": 42, "part_number": part},
		})
		if err != nil {
			t.Fatalf("CallTool %s: %v", part, err)
		}
		if len(res.Content) != 2 {
			t.Fatalf("part %s: %d content blocks, want 2", part, len(res.Content))
		}
		return res
	}

	if er, ok := call("2").Content[1].(*mcp.EmbeddedResource); !ok || !bytes.Equal(er.Resource.Blob, mockPDF) ||
		er.Resource.MIMEType != "application/pdf" {
		t.Errorf("PDF over the wire = %#v", er)
	}
	if img, ok := call("3").Content[1].(*mcp.ImageContent); !ok || !bytes.Equal(img.Data, mockPNG) {
		t.Errorf("PNG over the wire = %#v", img)
	}
}
