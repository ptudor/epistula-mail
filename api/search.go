package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// GET /v1/search — full-text search over the fts tsvector (subject +
// text_body, weighted at ingest). Results are metadata-shaped, but the
// matching itself is content-shaped (q= can probe body words), so the
// handler requires read_content (R-014); consumers fetch the matched
// bodies with /v1/messages/{id}/text under the same permission.

// searchConds builds the shared scope + filter WHERE clause for search and
// export. Returns the conditions and bind args; the caller appends its own
// pagination terms. Writes the error response and returns ok=false on a
// bad parameter.
func (s *server) searchConds(w http.ResponseWriter, r *http.Request, tok *apiToken) (conds []string, args []any, ok bool) {
	all, scope := scopeParams(tok)
	args = []any{all, scope}
	conds = []string{"($1::bool OR mb.id = ANY($2::bigint[]))"}
	addCond := func(format string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}

	if mbName := r.URL.Query().Get("mailbox"); mbName != "" {
		mailboxID, found := s.lookupMailbox(w, r, mbName)
		if !found {
			return nil, nil, false
		}
		if !requireMailboxScope(w, r, tok, mailboxID, mbName) {
			return nil, nil, false
		}
		addCond("mb.id = $%d", mailboxID)
		if folder := r.URL.Query().Get("folder"); folder != "" {
			addCond("f.name = $%d", folder)
		}
	} else if r.URL.Query().Get("folder") != "" {
		problemUnprocessable(w, r, "folder requires mailbox")
		return nil, nil, false
	}

	if t, err := parseTimeParam(r, "since"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return nil, nil, false
	} else if !t.IsZero() {
		addCond("m.internal_date >= $%d", t)
	}
	if t, err := parseTimeParam(r, "before"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return nil, nil, false
	} else if !t.IsZero() {
		addCond("m.internal_date < $%d", t)
	}
	if !s.addAnnotationFilters(w, r, tok, &conds, &args) {
		return nil, nil, false
	}
	if !s.addArchiveFilters(w, r, "f.mailbox_id", &conds, &args) {
		return nil, nil, false
	}
	return conds, args, true
}

// datePageWindow applies the (internal_date DESC, id DESC) cursor window.
func datePageWindow(w http.ResponseWriter, r *http.Request, conds *[]string, args *[]any) bool {
	c := r.URL.Query().Get("cursor")
	if c == "" {
		return true
	}
	keys, err := decodeCursor(c, 2)
	if err != nil {
		problemUnprocessable(w, r, "Malformed cursor.")
		return false
	}
	*args = append(*args, time.UnixMicro(keys[0]).UTC())
	dateIdx := len(*args)
	*args = append(*args, keys[1])
	*conds = append(*conds, fmt.Sprintf("(m.internal_date, m.id) < ($%d, $%d)", dateIdx, len(*args)))
	return true
}

const messagePageSelect = `
	SELECT m.id, m.uid, m.internal_date, m.sent_date, m.subject, m.from_addr,
	       m.to_addrs, m.cc_addrs, m.message_id, m.in_reply_to, m.flags, m.raw_size,
	       (SELECT count(*) FROM attachments a WHERE a.message_id = m.id),
	       %s, mb.name, f.name
	  FROM messages m
	  JOIN folders f ON f.id = m.folder_id
	  JOIN mailboxes mb ON mb.id = f.mailbox_id
	 WHERE %s
	 ORDER BY m.internal_date DESC, m.id DESC
	 LIMIT $%d`

// scanDatePage scans the messagePageSelect shape (the 14 canonical columns
// plus mailbox and folder names).
func (s *server) scanDatePage(r *http.Request, query string, args []any) ([]messageItem, error) {
	items, _, err := s.scanDatePageBounded(r, query, args, 0)
	return items, err
}

// scanDatePageBounded is scanDatePage with an aggregate retained-BYTES budget
// (RA6X-040). It reports truncated=true when it stopped early because the page
// was full rather than because the query ran out of rows.
//
// A budget of 0 means unbounded, which is what a metadata-only page gets: its
// rows are small and fixed-shaped, so bounding them by count is already
// bounding them by size.
//
// Stopping early is safe precisely because these pages are cursor-paged on
// (internal_date, id): the caller resumes from the last row it kept, so every
// row still arrives exactly once and in the same order. Nothing is dropped and
// no body is truncated — only the size of one in-memory batch changes.
func (s *server) scanDatePageBounded(r *http.Request, query string, args []any, maxBytes int64) ([]messageItem, bool, error) {
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var budget *pageBudget
	if maxBytes > 0 {
		budget = newPageBudget(maxBytes)
	}

	items := []messageItem{}
	truncated := false
	for rows.Next() {
		var m messageItem
		if err := rows.Scan(&m.ID, &m.UID, &m.InternalDate, &m.SentDate, &m.Subject, &m.From,
			&m.To, &m.Cc, &m.MessageID, &m.InReplyTo, &m.Flags, &m.Size,
			&m.AttachmentCount, &m.TextBody, &m.Mailbox, &m.Folder); err != nil {
			return nil, false, err
		}
		if budget != nil && !budget.admit(itemBytes(&m)) {
			truncated = true
			break
		}
		items = append(items, m)
	}
	if truncated {
		// Stop reading the rest of the result set: the remaining rows are the
		// next batch's, and draining them here is exactly the memory this is
		// avoiding.
		return items, true, nil
	}
	return items, false, rows.Err()
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	// The fts tsvector indexes text_body (weight B), so a `q=` probe confirms
	// arbitrary words/phrases in message bodies — the exact content inference
	// read_content is meant to gate. Require read_content (results themselves
	// stay metadata-shaped). (R-014)
	if !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		problemUnprocessable(w, r, "q is required")
		return
	}
	limit, err := s.pageSize(r)
	if err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}

	conds, args, ok := s.searchConds(w, r, tok)
	if !ok {
		return
	}
	args = append(args, q)
	conds = append(conds, fmt.Sprintf("m.fts @@ plainto_tsquery('simple', $%d)", len(args)))
	if !datePageWindow(w, r, &conds, &args) {
		return
	}

	args = append(args, limit+1)
	query := fmt.Sprintf(messagePageSelect, "NULL", strings.Join(conds, " AND "), len(args))
	items, err := s.scanDatePage(r, query, args)
	if err != nil {
		slog.Error("search query", "err", err)
		problemInternal(w, r)
		return
	}

	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		nextCursor = encodeCursor(last.InternalDate.UnixMicro(), last.ID)
	}

	resp := map[string]any{"q": q, "messages": items}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	writeJSON(w, http.StatusOK, resp)
}
