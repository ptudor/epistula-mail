package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
)

// Single-message endpoints: full document, plaintext projection, raw bytes.

// safeShortSHA truncates a hex sha256 to 16 chars for logging without
// assuming the column holds a full-length digest — a store already corrupt
// enough to reach these error paths is exactly where an unguarded
// `sha[:16]` would panic. Mirrors imapsess.safeShortSHA (RO5X-029).
func safeShortSHA(s string) string {
	if len(s) >= 16 {
		return s[:16]
	}
	return s
}

type attachmentItem struct {
	PartNumber  string  `json:"part_number"`
	Filename    *string `json:"filename,omitempty"`
	ContentType string  `json:"content_type"`
	ContentID   *string `json:"content_id,omitempty"`
	Disposition *string `json:"disposition,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
}

type messageDoc struct {
	messageItem
	Headers       json.RawMessage  `json:"headers"`
	HTMLBody      *string          `json:"html_body,omitempty"`
	BodyStructure json.RawMessage  `json:"bodystructure"`
	Attachments   []attachmentItem `json:"attachments"`
	// TextBytes and TextTruncated appear only when ?text_limit= bounded the
	// inlined body, so a consumer that asked for a preview can tell how much
	// it did not get and page the rest through /text (RA6X-046).
	TextBytes      *int64 `json:"text_bytes,omitempty"`
	TextTruncated  bool   `json:"text_truncated,omitempty"`
	NextAttachment string `json:"next_attachment,omitempty"`
	NextAnnotation string `json:"next_annotation,omitempty"`
}

// bodyProjection is the optional, additive bound a caller can put on the
// single-message document's inlined bodies (RA6X-046).
//
// The document inlines the full text AND html bodies, so a message with more
// than the connector's response budget of serialized body failed the whole
// request — even when the caller only wanted its metadata, its attachment list
// or a short preview. The paged /text endpoint does not repair that, because
// the metadata lives in this document.
//
// Both parameters are absent by default and the default response is byte-for-
// byte what it was, so every other consumer is unaffected. Raising the
// connector's response cap instead was rejected: that reproduces the memory
// problem one message at a time, which is the thing the cap exists to prevent.
type bodyProjection struct {
	// textLimit bounds the inlined text_body in BYTES. 0 means unbounded.
	textLimit int
	// includeHTML keeps html_body in the document. Default true.
	includeHTML bool
	inspection  bool
}

func parseBodyProjection(w http.ResponseWriter, r *http.Request) (bodyProjection, bool) {
	p := bodyProjection{includeHTML: true}
	limit, ok := intQueryParam(w, r, "text_limit", 0)
	if !ok {
		return p, false
	}
	if limit < 0 {
		problemUnprocessable(w, r, "text_limit must be non-negative.")
		return p, false
	}
	p.textLimit = min(limit, 1<<30)
	switch r.URL.Query().Get("metadata") {
	case "":
	case "inspection":
		p.inspection = true
	default:
		problemUnprocessable(w, r, "metadata must be inspection or absent.")
		return p, false
	}
	if p.inspection {
		if p.textLimit == 0 {
			p.textLimit = 64 * 1024
		}
		p.includeHTML = false
	}
	switch v := strings.TrimSpace(r.URL.Query().Get("html")); v {
	case "":
	case "0", "false":
		p.includeHTML = false
	case "1", "true":
		p.includeHTML = true
	default:
		problemUnprocessable(w, r, "html must be true or false.")
		return p, false
	}
	if p.inspection {
		p.includeHTML = false
	}
	if p.inspection {
		if raw := r.URL.Query().Get("summary_offset"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 || (n > 0 && r.URL.Query().Get("annotation_model") == "") {
				problemUnprocessable(w, r, "summary_offset must be non-negative and a positive offset requires annotation_model.")
				return p, false
			}
		}
	}
	return p, true
}

// apply bounds the document's bodies in place.
func (p bodyProjection) apply(doc *messageDoc) {
	if !p.includeHTML {
		doc.HTMLBody = nil
	}
	if p.textLimit <= 0 || doc.TextBody == nil {
		return
	}
	full := int64(len(*doc.TextBody))
	if doc.TextBytes != nil {
		full = *doc.TextBytes
	}
	_, slice := sliceTextBody(*doc.TextBody, 0, p.textLimit)
	if int64(len(slice)) < full {
		doc.TextTruncated = true
	}
	doc.TextBody = &slice
	doc.TextBytes = &full
}

// parseMessageID parses the {id} path value; a non-integer is a 404 (the
// resource space is integer ids — nothing else exists).
func parseMessageID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		problemNotFound(w, r, "No such message.")
		return 0, false
	}
	return id, true
}

// messageRef locates a message's owning mailbox/folder for scope checks
// plus the blob locator for raw streaming.
type messageRef struct {
	ID int64
	// MailboxID is the durable owner identity the scope check uses; Mailbox
	// is its current display name (RA6X-012).
	MailboxID   int64
	Mailbox     string
	Folder      string
	RawSHA256   string
	RawBlobDate time.Time
	RawSize     int64
	TextBody    *string
	TextTotal   int64
	TextBase    int64
}

// Two resolver query shapes. Only handleMessageText reads TextBody, so the
// other callers must not pull it: handleMessageRaw then streams the blob from
// disk (making the text pure waste on top of a file read), and
// handleAnnotationPut — the daemon's single WRITE path — used to pull the full
// decoded body of a message purely to learn the owning mailbox name.
//
// For the LLM worker's steady state (annotate everything it exported) that
// doubled the body bytes crossing the wire per message: once in the export
// stream, once again in the PUT's scope lookup. It also weakened the
// least-privilege story, since a write_annotation-only consumer's request
// still read message content into shared buffers and pg_stat_statements
// (RO5X-022).
const (
	messageRefSelect = `
		SELECT mb.id, mb.name, f.name, encode(m.raw_sha256, 'hex'), m.raw_blob_date, m.raw_size
		  FROM messages m
		  JOIN folders f ON f.id = m.folder_id
		  JOIN mailboxes mb ON mb.id = f.mailbox_id
		 WHERE m.id = $1`

	messageRefWithTextSelect = `
		SELECT mb.id, mb.name, f.name, encode(m.raw_sha256, 'hex'), m.raw_blob_date, m.raw_size,
            substring(convert_to(m.text_body,'UTF8') FROM LEAST($2::bigint,COALESCE(octet_length(m.text_body),0))::integer+1 FOR $3::integer),
            COALESCE(octet_length(m.text_body),0)::bigint
		  FROM messages m
		  JOIN folders f ON f.id = m.folder_id
		  JOIN mailboxes mb ON mb.id = f.mailbox_id
		 WHERE m.id = $1`
)

// resolveMessage loads the scope-relevant facts for one message, WITHOUT the
// decoded body. Writes 404 and returns nil when the id does not exist.
func (s *server) resolveMessage(w http.ResponseWriter, r *http.Request, id int64) *messageRef {
	ref := &messageRef{ID: id}
	err := s.pool.QueryRow(r.Context(), messageRefSelect, id).
		Scan(&ref.MailboxID, &ref.Mailbox, &ref.Folder, &ref.RawSHA256, &ref.RawBlobDate, &ref.RawSize)
	return s.finishResolve(w, r, id, ref, err)
}

// resolveMessageWithText is resolveMessage plus text_body, for the one handler
// that actually serves it.
func (s *server) resolveMessageWithText(w http.ResponseWriter, r *http.Request, id int64, offset, limit int) *messageRef {
	ref := &messageRef{ID: id}
	base := int64(max(offset-3, 0))
	size := 1 << 30
	if limit > 0 {
		size = min(limit, (1<<30)-7) + 7
	}
	var raw []byte
	err := s.pool.QueryRow(r.Context(), messageRefWithTextSelect, id, base, size).
		Scan(&ref.MailboxID, &ref.Mailbox, &ref.Folder, &ref.RawSHA256, &ref.RawBlobDate, &ref.RawSize, &raw, &ref.TextTotal)
	body := string(raw)
	ref.TextBody = &body
	ref.TextBase = min(base, ref.TextTotal)
	return s.finishResolve(w, r, id, ref, err)
}

// finishResolve maps a resolve error onto the response. Shared so the
// 404-on-ErrNoRows behaviour is identical across both forms — the 403-vs-404
// contract (and TestAPIOutOfScopeIs403) depends on the resolve succeeding
// before the scope check.
func (s *server) finishResolve(w http.ResponseWriter, r *http.Request, id int64, ref *messageRef, err error) *messageRef {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problemNotFound(w, r, "No such message.")
			return nil
		}
		slog.Error("message resolve", "message_id", id, "err", err)
		problemInternal(w, r)
		return nil
	}
	return ref
}

// ---- GET /v1/messages/{id} ----

func (s *server) handleMessage(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	// The full document inlines text/html bodies, so the content
	// permission gates the whole endpoint.
	if !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}
	id, ok := parseMessageID(w, r)
	if !ok {
		return
	}
	projection, ok := parseBodyProjection(w, r)
	if !ok {
		return
	}

	var (
		doc       messageDoc
		mailboxID int64
		mailbox   string
		folder    string
	)
	err := s.pool.QueryRow(r.Context(),
		`SELECT m.id, m.uid, mb.id, mb.name, f.name, m.internal_date, m.sent_date, m.subject, m.from_addr,
		        m.to_addrs, m.cc_addrs, m.message_id, m.in_reply_to, m.flags, m.raw_size,
		        CASE WHEN $4 THEN NULL::jsonb ELSE m.headers END,
                CASE WHEN $2>0 THEN left(m.text_body,$2) ELSE m.text_body END,
                CASE WHEN $3 THEN m.html_body ELSE NULL END,
                CASE WHEN $4 THEN NULL::jsonb ELSE m.bodystructure END,
                octet_length(m.text_body)::bigint,
                (SELECT count(*) FROM attachments a WHERE a.message_id=m.id)
		   FROM messages m
		   JOIN folders f ON f.id = m.folder_id
		   JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE m.id = $1`, id, projection.textLimit, projection.includeHTML, projection.inspection,
	).Scan(&doc.ID, &doc.UID, &mailboxID, &mailbox, &folder, &doc.InternalDate, &doc.SentDate, &doc.Subject, &doc.From,
		&doc.To, &doc.Cc, &doc.MessageID, &doc.InReplyTo, &doc.Flags, &doc.Size,
		&doc.Headers, &doc.TextBody, &doc.HTMLBody, &doc.BodyStructure, &doc.TextBytes, &doc.AttachmentCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problemNotFound(w, r, "No such message.")
			return
		}
		slog.Error("message query", "message_id", id, "err", err)
		problemInternal(w, r)
		return
	}
	if !requireMailboxScope(w, r, tok, mailboxID, mailbox) {
		return
	}
	doc.Mailbox = mailbox
	doc.Folder = folder

	if projection.inspection {
		if !s.annotationsAvailable {
			problemUnavailable(w, r, "Annotations require the message_annotations migration in epistula-database.")
			return
		}
		if err := s.inspectionMetadata(r, &doc); err != nil {
			slog.Error("inspection metadata", "err", err)
			problemInternal(w, r)
			return
		}
		projection.apply(&doc)
		writeJSON(w, http.StatusOK, &doc)
		return
	}
	// Full-document callers retain the original response shape.
	if projection.textLimit == 0 {
		doc.TextBytes = nil
	}
	doc.Attachments = []attachmentItem{}
	rows, err := s.pool.Query(r.Context(),
		`SELECT part_number, filename, content_type, content_id, disposition, size_bytes,
		        encode(sha256, 'hex')
		   FROM attachments WHERE message_id = $1 ORDER BY part_number`, id)
	if err != nil {
		slog.Error("attachments query", "message_id", id, "err", err)
		problemInternal(w, r)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var a attachmentItem
		if err := rows.Scan(&a.PartNumber, &a.Filename, &a.ContentType, &a.ContentID,
			&a.Disposition, &a.SizeBytes, &a.SHA256); err != nil {
			slog.Error("attachments scan", "message_id", id, "err", err)
			problemInternal(w, r)
			return
		}
		doc.Attachments = append(doc.Attachments, a)
	}
	if err := rows.Err(); err != nil {
		slog.Error("attachments rows", "message_id", id, "err", err)
		problemInternal(w, r)
		return
	}

	// The single-message document always includes annotations. If the sidecar
	// migration isn't deployed, 503 rather than serve a document that silently
	// omits them — a consumer can't otherwise distinguish "not annotated" from
	// "sidecar not deployed" (R-066).
	if !s.annotationsAvailable {
		problemUnavailable(w, r, "Annotations require the message_annotations migration in epistula-database.")
		return
	}
	annots, err := s.loadAnnotations(r.Context(), []int64{id})
	if err != nil {
		slog.Error("message annotations", "message_id", id, "err", err)
		problemInternal(w, r)
		return
	}
	doc.Annotations = annots[id]

	doc.AttachmentCount = int64(len(doc.Attachments))
	projection.apply(&doc)
	writeJSON(w, http.StatusOK, &doc)
}

// ---- GET /v1/messages/{id}/text ----

func (s *server) handleMessageText(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}
	id, ok := parseMessageID(w, r)
	if !ok {
		return
	}
	offset, ok := intQueryParam(w, r, "offset", 0)
	if !ok {
		return
	}
	limit, ok := intQueryParam(w, r, "limit", 0)
	if !ok {
		return
	}
	if offset < 0 || limit < 0 {
		problemUnprocessable(w, r, "offset and limit must be non-negative.")
		return
	}

	ref := s.resolveMessageWithText(w, r, id, offset, limit)
	if ref == nil {
		return
	}
	if !requireMailboxScope(w, r, tok, ref.MailboxID, ref.Mailbox) {
		return
	}
	body := ""
	if ref.TextBody != nil {
		body = *ref.TextBody
	}

	// Optional byte-range projection: ?offset=&limit=.
	//
	// Additive to the /v1 contract and both default to "whole body", so
	// existing consumers are unaffected. Without it a paging client had to
	// re-fetch the entire body for every page — 16 full-body transfers and
	// 16 full-body allocations to walk 1 MiB at a 64 KiB page size, which
	// is O(n^2) in the body length (RO5X-016).
	//
	// The slice is trimmed back to a UTF-8 rune boundary so a multibyte
	// character is never split across pages, matching the connector's own
	// capText back-off.
	local, slice := sliceTextBody(body, int(min(int64(offset), ref.TextTotal)-ref.TextBase), limit)
	start := ref.TextBase + int64(local)

	// X-Total-Bytes always reports the FULL body length, so a client can
	// reason about progress without another round trip.
	w.Header().Set("X-Total-Bytes", strconv.FormatInt(ref.TextTotal, 10))
	// X-Content-Offset reports the EFFECTIVE start of the slice (RA6X-018).
	//
	// An offset that lands inside a multibyte rune is advanced to the next
	// rune boundary, and the client had no way to learn that: it computed its
	// next cursor from the offset it SENT, so a request at an interior byte
	// either re-read bytes it had already seen or, at a small page size,
	// produced a cursor that never advanced. The effective start is now part
	// of the response, and a client that echoes it forward always makes
	// progress.
	w.Header().Set("X-Content-Offset", strconv.FormatInt(start, 10))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, slice)
}

// intQueryParam parses a non-negative integer query parameter, writing a 422
// and returning ok=false on a malformed value. An absent parameter yields def.
func intQueryParam(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		problemUnprocessable(w, r, name+" must be an integer.")
		return 0, false
	}
	return n, true
}

// sliceTextBody returns up to limit bytes of body starting at offset, trimmed
// back to a UTF-8 rune boundary at both ends so a multibyte character is never
// split. limit <= 0 means "to the end". An offset past the end yields "".
func sliceTextBody(body string, offset, limit int) (start int, slice string) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(body) {
		return len(body), ""
	}
	// Advance to a rune start so a caller resuming from a byte offset it
	// computed elsewhere cannot begin mid-rune. The adjusted value is returned
	// so the caller can report it (RA6X-018).
	for offset < len(body) && !utf8.RuneStart(body[offset]) {
		offset++
	}
	end := len(body)
	if limit > 0 && len(body)-offset > limit {
		end = offset + limit
		for end > offset && !utf8.RuneStart(body[end]) {
			end--
		}
		if end == offset {
			// The next rune is wider than the whole budget. Returning an empty
			// slice here is what made a small page size a cursor that never
			// advanced: the client saw zero bytes, computed the same offset
			// again, and looped forever. One COMPLETE rune is emitted instead,
			// overshooting the budget by at most three bytes — a bounded
			// overshoot beats an unbounded loop, and a rune is the smallest
			// unit this projection can honestly hand out.
			_, size := utf8.DecodeRuneInString(body[offset:])
			end = offset + size
		}
	}
	return offset, body[offset:end]
}

// ---- GET /v1/messages/{id}/raw ----

func (s *server) handleMessageRaw(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}
	id, ok := parseMessageID(w, r)
	if !ok {
		return
	}
	ref := s.resolveMessage(w, r, id)
	if ref == nil {
		return
	}
	if !requireMailboxScope(w, r, tok, ref.MailboxID, ref.Mailbox) {
		return
	}

	// The blob tenant is the owning mailbox (ref.Mailbox = mb.name). It comes
	// from the DB, which constrains names to the path-safe charset; a parse
	// failure is store/schema corruption, surfaced as an internal error.
	tenant, terr := blob.ParseTenant(ref.Mailbox)
	if terr != nil {
		slog.Error("raw blob tenant invalid", "message_id", id,
			"mailbox", ref.Mailbox, "err", terr)
		problemInternal(w, r)
		return
	}
	bucket := blob.BucketFromTime(ref.RawBlobDate)
	rc, err := s.store.Open(blob.KindRaw, tenant, bucket, ref.RawSHA256)
	if err != nil {
		// A DB row whose blob is gone is store corruption — same
		// operator-paging condition the IMAP server raises as SERVERBUG.
		slog.Error("raw blob open", "message_id", id,
			"sha16", safeShortSHA(ref.RawSHA256), "bucket", bucket, "err", err)
		problemInternal(w, r)
		return
	}
	defer rc.Close()

	// Content-Length is announced from the DB's raw_size but the bytes come
	// off disk. If they disagree the response is malformed either way:
	// disk < DB leaves the client blocking for bytes that never arrive
	// (a stalled Apache proxy worker), and disk > DB makes net/http refuse
	// the overflow, so io.Copy fails with ErrContentLength after a
	// truncated message/rfc822 has already gone out — which the consumer
	// re-parses as a different message. Refuse before writing any header.
	//
	// blob.Store.Open returns an *os.File, so the assertion holds; if Stat
	// itself fails, stream anyway rather than failing closed on an
	// unrelated errno. This is the R-060 check epistula-imap already
	// applies to the same blobs (RO5X-005).
	if statter, ok := rc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if fi, statErr := statter.Stat(); statErr == nil && fi.Size() != ref.RawSize {
			slog.Error("raw blob size mismatch", "message_id", id,
				"sha16", safeShortSHA(ref.RawSHA256),
				"db_size", ref.RawSize, "disk_size", fi.Size())
			problemInternal(w, r)
			return
		}
	}

	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Length", strconv.FormatInt(ref.RawSize, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Mid-stream failure: the status is already written; all we can
		// do is log and let the connection tear down.
		slog.Warn("raw blob stream", "message_id", id, "err", err)
	}
}
