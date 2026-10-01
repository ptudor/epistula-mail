package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

func microToTime(micro int64) time.Time { return time.UnixMicro(micro).UTC() }

// GET /v1/export — bulk NDJSON stream of messages matching a filter, for
// "summarize the whole archive" consumers. Rows stream as they are read;
// the corpus is never buffered. Internally the stream advances by
// cursor-paged batches (export_page_size per statement), so the session
// statement_timeout applies per batch rather than to the whole walk, and
// the no-OFFSET rule holds at any depth.

func (s *server) handleExport(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	// Export inlines text bodies — content permission required.
	if !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}

	// Server-wide cap on concurrent export streams. Non-blocking: a
	// saturated server says 429 immediately rather than queueing.
	select {
	case s.exportSem <- struct{}{}:
		defer func() { <-s.exportSem }()
	default:
		problemTooMany(w, r, "Export stream limit reached; retry later.")
		return
	}
	metricExportStreams.Inc()
	defer metricExportStreams.Dec()

	baseConds, baseArgs, ok := s.searchConds(w, r, tok)
	if !ok {
		return
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		baseArgs = append(baseArgs, q)
		baseConds = append(baseConds, fmt.Sprintf("m.fts @@ plainto_tsquery('simple', $%d)", len(baseArgs)))
	}
	includeAnnotations := s.annotationsAvailable

	pageSize := s.cfg.Limits.ExportPageSize
	var afterDateMicro, afterID int64
	first := true

	// fetchBatch builds and runs one cursor-paged batch (plus its annotation
	// attach) for the current cursor state. Kept as a closure so the FIRST
	// batch can run before headers are committed.
	maxBytes := s.maxPageBytes()
	fetchBatch := func() ([]messageItem, bool, error) {
		conds := append([]string{}, baseConds...)
		args := append([]any{}, baseArgs...)
		if !first {
			args = append(args, microToTime(afterDateMicro))
			dateIdx := len(args)
			args = append(args, afterID)
			conds = append(conds, fmt.Sprintf("(m.internal_date, m.id) < ($%d, $%d)", dateIdx, len(args)))
		}
		args = append(args, pageSize)
		query := fmt.Sprintf(messagePageSelect, "m.text_body", strings.Join(conds, " AND "), len(args))

		// Bounded by retained BYTES, not just row count (RA6X-040). A batch
		// cut short here resumes from its own cursor below, so the stream
		// stays complete and ordered.
		items, truncated, err := s.scanDatePageBounded(r, query, args, maxBytes)
		if err != nil {
			return nil, false, err
		}
		if len(items) > 0 {
			var attachmentTruncated bool
			items, attachmentTruncated, err = s.attachAttachments(r.Context(), items)
			if err != nil {
				return nil, false, err
			}
			truncated = truncated || attachmentTruncated
		}
		if includeAnnotations && len(items) > 0 {
			var annotationTruncated bool
			items, annotationTruncated, err = s.attachAnnotations(r.Context(), items)
			if err != nil {
				return nil, false, err
			}
			truncated = truncated || annotationTruncated
		}
		return items, truncated, nil
	}

	// Run the first batch BEFORE writing any header so an immediate backend
	// failure returns a proper RFC 7807 5xx instead of a 200 + in-band error
	// row (which a consumer can only detect heuristically). Only after the
	// first batch succeeds do we commit 200 + NDJSON; later batch failures are
	// necessarily in-band because the header is already sent.
	items, truncated, err := fetchBatch()
	if err != nil {
		slog.Error("export first batch", "err", err)
		problemInternal(w, r)
		return
	}

	progress := &exportProgressWriter{w: w, rc: http.NewResponseController(w), stall: s.exportWriteStall()}
	if err := progress.refresh(); err != nil {
		slog.Error("export write deadline unavailable", "err", err)
		problemUnavailable(w, r, "This connection cannot enforce the export write-progress timeout.")
		return
	}
	defer func() { _ = progress.rc.SetWriteDeadline(time.Time{}) }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(progress)

	total := 0
	for {
		if len(items) == 0 {
			return
		}
		for i := range items {
			if err := enc.Encode(&items[i]); err != nil {
				// The client went away, or stopped reading for longer than the
				// stall budget. Either way the stream is over and returning
				// releases the semaphore slot. This is NOT reported as a
				// complete stream: the consumer saw a truncated body with no
				// terminator, which is exactly what happened.
				slog.Info("export stream ended before completion",
					"rows", total, "token", tok.Name, "err", err)
				panic(http.ErrAbortHandler)
			}
			total++
		}
		if err := progress.Flush(); err != nil {
			slog.Info("export flush failed", "err", err)
			panic(http.ErrAbortHandler)
		}
		last := items[len(items)-1]
		afterDateMicro, afterID = last.InternalDate.UnixMicro(), last.ID
		// A short batch means the query ran out of rows — UNLESS it was cut
		// short by the byte budget, in which case there is more to come from
		// the same cursor (RA6X-040).
		if len(items) < pageSize && !truncated {
			return
		}
		if r.Context().Err() != nil {
			return
		}
		first = false
		items, truncated, err = fetchBatch()
		if err != nil {
			// Headers are committed; surface the failure in-band so the
			// consumer can distinguish truncation from completion.
			slog.Error("export batch", "err", err)
			if err := enc.Encode(map[string]string{"error": "export aborted: backend failure"}); err != nil {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

// attachAttachments adds each exported message's attachment metadata: the
// objects GET /v1/messages/{id} lists, fetched for the whole batch in one query
// (OPS-009). The annotation worker reads the export, and without them it saw
// only a count, so an archive named "RECIBO DE PAGO.rar" passed as a genuine
// receipt. The rows count against the page budget like the message itself; a
// batch cut short here resumes from its own cursor.
func (s *server) attachAttachments(ctx context.Context, items []messageItem) ([]messageItem, bool, error) {
	ids := make([]int64, len(items))
	pos := make(map[int64]int, len(items))
	for i := range items {
		ids[i] = items[i].ID
		pos[items[i].ID] = i
	}
	rows, err := s.pool.Query(ctx,
		`SELECT message_id, part_number, filename, content_type, content_id, disposition,
		        size_bytes, encode(sha256, 'hex')
		   FROM attachments WHERE message_id = ANY($1)
		  ORDER BY message_id, part_number`, ids)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a attachmentItem
		if err := rows.Scan(&id, &a.PartNumber, &a.Filename, &a.ContentType, &a.ContentID,
			&a.Disposition, &a.SizeBytes, &a.SHA256); err != nil {
			return nil, false, err
		}
		i := pos[id]
		items[i].Attachments = append(items[i].Attachments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	budget := newPageBudget(s.maxPageBytes())
	for i := range items {
		if !budget.admit(itemBytes(&items[i])) {
			clear(items[i:]) // release bodies backing the deferred tail
			return items[:i], true, nil
		}
	}
	return items, false, nil
}

// exportWriteStall is the configured rolling write-progress budget for an
// export stream. An empty setting selects the default; "0s" disables the bound
// (RA6X-063).
func (s *server) exportWriteStall() time.Duration {
	if s.cfg.Limits.ExportWriteStall == "" {
		return defaultExportWriteStall
	}
	d, err := time.ParseDuration(s.cfg.Limits.ExportWriteStall)
	if err != nil || d < 0 {
		// Validated at startup; a bad value here can only come from a test
		// constructing a config by hand, and the safe reading is the default.
		return defaultExportWriteStall
	}
	return d
}

// defaultExportWriteStall is coordinated with epistula-llm-worker's own bounds
// (RA6X-014, RA6X-042). The worker holds this stream open across per-message
// LLM inference and does not read while it runs, so the budget must exceed the
// worker's WORST CASE for one message:
//
//	(lmstudio.request_timeout_seconds x (retry_attempts + 1)) +
//	(worker.retry_backoff x retry_attempts)
//
// which with the worker's shipped defaults (300s, 2 retries, 2s backoff) is
// 904 seconds — about fifteen minutes. Thirty minutes leaves that a wide
// margin while still being far inside "this reader is never coming back", and
// an operator who raises the worker's model timeout should raise this with it.
const defaultExportWriteStall = 30 * time.Minute
