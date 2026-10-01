package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/imapflags"
)

// ---- shared response shapes ----

type mailboxItem struct {
	Name      string `json:"name"`
	UsedBytes int64  `json:"used_bytes"`
	// QuotaBytes is 0 for unlimited mailboxes.
	QuotaBytes int64 `json:"quota_bytes"`
	Disabled   bool  `json:"disabled"`
}

type folderItem struct {
	Name         string `json:"name"`
	UIDValidity  int64  `json:"uidvalidity"`
	UIDNext      int64  `json:"uidnext"`
	SpecialUse   string `json:"special_use,omitempty"`
	MessageCount int64  `json:"message_count"`
}

type annotationItem struct {
	SummaryBytes      *int64    `json:"summary_bytes,omitempty"`
	SummaryOffset     *int64    `json:"summary_offset,omitempty"`
	SummaryNextOffset *int64    `json:"summary_next_offset,omitempty"`
	Model             string    `json:"model"`
	Tags              []string  `json:"tags"`
	Category          *string   `json:"category,omitempty"`
	Summary           *string   `json:"summary,omitempty"`
	TokensIn          *int64    `json:"tokens_in,omitempty"`
	TokensOut         *int64    `json:"tokens_out,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	// Priority and Primary are populated only when the annotation_models
	// registry is present. Annotations are returned highest-priority first;
	// Primary marks the one a consumer should display, the rest being
	// retained alternatives. A retired model is never primary.
	//
	// Priority is a *int so a ranked priority of 0 (an unregistered model, or a
	// model explicitly set to priority 0) is distinguishable on the wire from
	// the registry-absent case: the ranked path always populates it (including
	// zero), while the unranked path leaves it nil so `omitempty` drops the key
	// entirely — a plain `int` collapsed both to "no priority field" (R-067).
	Priority *int `json:"priority,omitempty"`
	Primary  bool `json:"primary,omitempty"`
}

type messageItem struct {
	ID              int64      `json:"id"`
	UID             int64      `json:"uid"`
	Mailbox         string     `json:"mailbox,omitempty"`
	Folder          string     `json:"folder,omitempty"`
	InternalDate    time.Time  `json:"internal_date"`
	SentDate        *time.Time `json:"sent_date,omitempty"`
	Subject         *string    `json:"subject,omitempty"`
	From            *string    `json:"from,omitempty"`
	To              []string   `json:"to,omitempty"`
	Cc              []string   `json:"cc,omitempty"`
	MessageID       *string    `json:"message_id,omitempty"`
	InReplyTo       *string    `json:"in_reply_to,omitempty"`
	Flags           []string   `json:"flags"`
	Size            int64      `json:"size"`
	AttachmentCount int64      `json:"attachment_count"`
	// Attachments is filled by /v1/export only (OPS-009). The single-message
	// document lists them through its own field, which shadows this one.
	Attachments []attachmentItem `json:"attachments,omitempty"`
	TextBody    *string          `json:"text_body,omitempty"`
	Annotations []annotationItem `json:"annotations,omitempty"`
}

// ---- shared helpers ----

// scopeParams renders the token's mailbox scope as the ($all, $list) pair
// every scope-filtered query binds. The SQL shape is always
// `($N::bool OR mb.id = ANY($M::bigint[]))` — filtering happens in the
// database, never after the fact in Go.
//
// The list is durable mailbox IDs, not names (RA6X-012). This is what makes a
// cached token safe across an account being deleted and its name reused: the
// cached IDs are permanently dead, so the predicate matches nothing no matter
// how warm the verification cache is.
func scopeParams(tok *apiToken) (bool, []int64) {
	if tok.AllMailboxes {
		return true, []int64{}
	}
	return false, tok.ScopeMailboxIDs
}

// pageSize resolves the limit query parameter against the configured
// default and cap. An unparseable or non-positive value is a 422.
func (s *server) pageSize(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return s.cfg.Limits.DefaultPageSize, nil
	}
	// Additive default:<cap> negotiates the configured default and a caller
	// cap in one request, without assuming the shipped default is deployed.
	useDefault := strings.HasPrefix(raw, "default:")
	if useDefault {
		raw = strings.TrimPrefix(raw, "default:")
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if useDefault && n > s.cfg.Limits.DefaultPageSize {
		n = s.cfg.Limits.DefaultPageSize
	}
	if n > s.cfg.Limits.MaxPageSize {
		n = s.cfg.Limits.MaxPageSize
	}
	return n, nil
}

// parseTimeParam accepts RFC 3339 or a bare date (interpreted as UTC
// midnight). Returns the zero time for an absent parameter.
func parseTimeParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%s must be RFC 3339 or YYYY-MM-DD", name)
}

// fieldsParam parses the `fields` projection list. metadata is implicit;
// text and annotation opt in to heavier payloads.
type fieldSet struct {
	text       bool
	annotation bool
}

func parseFields(r *http.Request) (fieldSet, error) {
	var fs fieldSet
	raw := r.URL.Query().Get("fields")
	if raw == "" {
		return fs, nil
	}
	for _, f := range strings.Split(raw, ",") {
		switch strings.TrimSpace(f) {
		case "", "metadata":
		case "text":
			fs.text = true
		case "annotation":
			fs.annotation = true
		default:
			return fs, fmt.Errorf("unknown field %q (valid: metadata, text, annotation)", f)
		}
	}
	return fs, nil
}

// loadAnnotations fetches the annotation sidecar rows for a page of
// message ids in one query. With the annotation_models registry present, rows
// come back ranked by model priority (see loadAnnotationsRanked); without it,
// in stable model-name order.
func (s *server) loadAnnotations(ctx context.Context, ids []int64) (map[int64][]annotationItem, error) {
	if !s.annotationsAvailable || len(ids) == 0 {
		return map[int64][]annotationItem{}, nil
	}
	if s.modelPriorityAvailable {
		return s.loadAnnotationsRanked(ctx, ids)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT message_id, model, tags, category, summary, tokens_in, tokens_out, created_at
		   FROM message_annotations
		  WHERE message_id = ANY($1)
		  ORDER BY message_id, model`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]annotationItem)
	for rows.Next() {
		var (
			msgID int64
			a     annotationItem
		)
		if err := rows.Scan(&msgID, &a.Model, &a.Tags, &a.Category, &a.Summary,
			&a.TokensIn, &a.TokensOut, &a.CreatedAt); err != nil {
			return nil, err
		}
		out[msgID] = append(out[msgID], a)
	}
	return out, rows.Err()
}

// loadAnnotationsRanked is loadAnnotations when the annotation_models registry
// is present. Each message's annotations are ordered (non-retired first,
// priority DESC, newest first); the top of each group is flagged Primary
// unless it is retired — so a consumer reads annotations[0] (or primary==true)
// for the preferred summary and keeps the rest as alternatives.
func (s *server) loadAnnotationsRanked(ctx context.Context, ids []int64) (map[int64][]annotationItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.message_id, a.model, a.tags, a.category, a.summary, a.tokens_in, a.tokens_out,
		        a.created_at, COALESCE(am.priority, 0), (am.retired_at IS NOT NULL)
		   FROM message_annotations a
		   LEFT JOIN annotation_models am ON am.model = a.model
		  WHERE a.message_id = ANY($1)
		  ORDER BY a.message_id,
		           (am.retired_at IS NOT NULL),
		           COALESCE(am.priority, 0) DESC,
		           a.created_at DESC,
		           a.model`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]annotationItem)
	var lastID int64
	haveLast := false
	for rows.Next() {
		var (
			msgID   int64
			a       annotationItem
			retired bool
			prio    int
		)
		if err := rows.Scan(&msgID, &a.Model, &a.Tags, &a.Category, &a.Summary,
			&a.TokensIn, &a.TokensOut, &a.CreatedAt, &prio, &retired); err != nil {
			return nil, err
		}
		// Ranked path always sets Priority (incl. 0) so the wire distinguishes
		// "ranked at zero" from the unranked path's nil (R-067). prio is a fresh
		// per-iteration variable, so &prio is a distinct address each row.
		a.Priority = &prio
		// First row of each message group is the top-ranked; mark it primary
		// unless it is retired.
		if !haveLast || msgID != lastID {
			a.Primary = !retired
			lastID = msgID
			haveLast = true
		}
		out[msgID] = append(out[msgID], a)
	}
	return out, rows.Err()
}

// ---- GET /v1/mailboxes ----

func (s *server) handleMailboxes(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadMetadata) {
		return
	}
	all, scope := scopeParams(tok)
	rows, err := s.pool.Query(r.Context(),
		`SELECT name, used_bytes, COALESCE(quota_bytes, 0), disabled_at IS NOT NULL
		   FROM mailboxes mb
		  WHERE ($1::bool OR mb.id = ANY($2::bigint[]))
		  ORDER BY name`, all, scope)
	if err != nil {
		slog.Error("mailboxes query", "err", err)
		problemInternal(w, r)
		return
	}
	defer rows.Close()
	out := []mailboxItem{}
	for rows.Next() {
		var m mailboxItem
		if err := rows.Scan(&m.Name, &m.UsedBytes, &m.QuotaBytes, &m.Disabled); err != nil {
			slog.Error("mailboxes scan", "err", err)
			problemInternal(w, r)
			return
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		slog.Error("mailboxes rows", "err", err)
		problemInternal(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": out})
}

// ---- GET /v1/mailboxes/{mailbox}/folders ----

func (s *server) handleFolders(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadMetadata) {
		return
	}
	mailbox := r.PathValue("mailbox")
	mailboxID, ok := s.lookupMailbox(w, r, mailbox)
	if !ok {
		return
	}
	if !requireMailboxScope(w, r, tok, mailboxID, mailbox) {
		return
	}
	rows, err := s.pool.Query(r.Context(),
		`SELECT f.name, f.uidvalidity, f.uidnext, COALESCE(f.special_use, ''),
		        (SELECT count(*) FROM messages WHERE folder_id = f.id)
		   FROM folders f
		  WHERE f.mailbox_id = $1
		  ORDER BY f.name`, mailboxID)
	if err != nil {
		slog.Error("folders query", "err", err)
		problemInternal(w, r)
		return
	}
	defer rows.Close()
	out := []folderItem{}
	for rows.Next() {
		var f folderItem
		if err := rows.Scan(&f.Name, &f.UIDValidity, &f.UIDNext, &f.SpecialUse, &f.MessageCount); err != nil {
			slog.Error("folders scan", "err", err)
			problemInternal(w, r)
			return
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		slog.Error("folders rows", "err", err)
		problemInternal(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailbox": mailbox, "folders": out})
}

func (s *server) lookupMailbox(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	var id int64
	err := s.pool.QueryRow(r.Context(),
		`SELECT id FROM mailboxes WHERE name = $1`, name).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problemNotFound(w, r, "Mailbox '"+name+"' does not exist.")
			return 0, false
		}
		slog.Error("mailbox lookup", "err", err)
		problemInternal(w, r)
		return 0, false
	}
	return id, true
}

// ---- GET /v1/mailboxes/{mailbox}/folders/{folder...}/messages ----

func (s *server) handleFolderMessages(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadMetadata) {
		return
	}
	mailbox := r.PathValue("mailbox")
	folder, ok := strings.CutSuffix(r.PathValue("rest"), "/messages")
	if !ok || folder == "" {
		problemNotFound(w, r, "No such endpoint (expected .../folders/{folder}/messages).")
		return
	}
	mailboxID, ok := s.lookupMailbox(w, r, mailbox)
	if !ok {
		return
	}
	if !requireMailboxScope(w, r, tok, mailboxID, mailbox) {
		return
	}

	fields, err := parseFields(r)
	if err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}
	// Both `text` and `annotation` expose body-derived content: annotation
	// rows carry `summary`, LLM prose over the full body (amounts, names,
	// receipt contents). So both require read_content — mirroring
	// GET /v1/messages/{id}, which gates its whole document (annotations
	// included) on read_content. A metadata-only token must not pull a
	// body-derived digest of every in-scope message. (R-012)
	if (fields.text || fields.annotation) && !requirePermission(w, r, tok, auth.PermissionReadContent) {
		return
	}
	// If annotations are requested but the sidecar migration isn't deployed,
	// 503 like the annotation filters do — otherwise the response silently
	// omits annotations and a consumer can't tell "not annotated" from "sidecar
	// not deployed" (R-066).
	if fields.annotation && !s.annotationsAvailable {
		problemUnavailable(w, r, "Annotations require the message_annotations migration in epistula-database.")
		return
	}
	limit, err := s.pageSize(r)
	if err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}

	var folderID int64
	err = s.pool.QueryRow(r.Context(),
		`SELECT f.id FROM folders f
		   JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE mb.name = $1 AND f.name = $2`, mailbox, folder).Scan(&folderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problemNotFound(w, r, "Folder '"+folder+"' does not exist in mailbox '"+mailbox+"'.")
			return
		}
		slog.Error("folder lookup", "err", err)
		problemInternal(w, r)
		return
	}

	// Cursor over (uid) ascending — natural IMAP order, never OFFSET.
	afterUID := int64(0)
	if c := r.URL.Query().Get("cursor"); c != "" {
		keys, err := decodeCursor(c, 1)
		if err != nil {
			problemUnprocessable(w, r, "Malformed cursor.")
			return
		}
		afterUID = keys[0]
	}

	conds := []string{"m.folder_id = $1", "m.uid > $2"}
	args := []any{folderID, afterUID}
	addCond := func(format string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}

	// Carry the token's mailbox scope in SQL, not just in Go.
	//
	// epistula-api/CLAUDE.md states the invariant plainly: "Every content query is
	// filtered by the token's mailbox scope at the SQL level
	// (WHERE mailbox_id = ANY($scope)), never in application code after the
	// fact." This handler was the one content endpoint that did not: it called
	// requireMailboxScope on the PATH parameter, resolved folderID from
	// (mb.name, f.name), then queried messages by folder_id alone. The result
	// is correct today — the folder id is derived from the checked mailbox
	// name — but it was one refactor away from being wrong, and it was the
	// only place where deleting a Go-level `if` would silently widen the
	// scope.
	//
	// Redundant by construction, which is the point (RO5X-039).
	allScope, scopeNames := scopeParams(tok)
	args = append(args, allScope)
	allIdx := len(args)
	args = append(args, scopeNames)
	namesIdx := len(args)
	conds = append(conds, fmt.Sprintf(
		`EXISTS (SELECT 1 FROM folders sf
		          WHERE sf.id = m.folder_id
		            AND ($%d::bool OR sf.mailbox_id = ANY($%d::bigint[])))`,
		allIdx, namesIdx))

	if t, err := parseTimeParam(r, "since"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	} else if !t.IsZero() {
		addCond("m.internal_date >= $%d", t)
	}
	if t, err := parseTimeParam(r, "before"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	} else if !t.IsZero() {
		addCond("m.internal_date < $%d", t)
	}
	if t, err := parseTimeParam(r, "sent_since"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	} else if !t.IsZero() {
		addCond("m.sent_date >= $%d", t)
	}
	if t, err := parseTimeParam(r, "sent_before"); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	} else if !t.IsZero() {
		addCond("m.sent_date < $%d", t)
	}
	// Flag filters. Canonicalize on read and validate the value.
	//
	// The comparison is byte-exact against messages.flags, so an
	// un-canonicalized filter silently disagreed with the IMAP server about
	// the same message: a row holding `\SEEN` (written before R-063, or by
	// any writer without a canonicalizer) was invisible to `flag=\Seen` and
	// *visible* to `not_flag=\Seen`, so the LLM worker and the MCP saw a
	// different unread set than the user's mail client. Rejecting a
	// malformed value with 422 also stops a consumer that passes `seen` or
	// `Seen` from silently receiving an empty page instead of an error
	// (RO5X-013).
	if f := r.URL.Query().Get("flag"); f != "" {
		if !imapflags.Valid(f) {
			problemUnprocessable(w, r, "flag must be a system flag or a keyword (non-empty, no whitespace, <= 64 bytes).")
			return
		}
		addCond("$%d = ANY(m.flags)", imapflags.Canonical(f))
	}
	if f := r.URL.Query().Get("not_flag"); f != "" {
		if !imapflags.Valid(f) {
			problemUnprocessable(w, r, "not_flag must be a system flag or a keyword (non-empty, no whitespace, <= 64 bytes).")
			return
		}
		addCond("NOT ($%d = ANY(m.flags))", imapflags.Canonical(f))
	}
	if v := r.URL.Query().Get("larger"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			problemUnprocessable(w, r, "larger must be a non-negative integer")
			return
		}
		addCond("m.raw_size > $%d", n)
	}
	if v := r.URL.Query().Get("smaller"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			problemUnprocessable(w, r, "smaller must be a non-negative integer")
			return
		}
		addCond("m.raw_size < $%d", n)
	}
	if ok := s.addAnnotationFilters(w, r, tok, &conds, &args); !ok {
		return
	}
	// This query does not join folders; every row is in folder $1, so its
	// mailbox is one value, looked up once.
	if ok := s.addArchiveFilters(w, r, "(SELECT lf.mailbox_id FROM folders lf WHERE lf.id = $1)", &conds, &args); !ok {
		return
	}

	textCol := "NULL"
	if fields.text {
		textCol = "m.text_body"
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT m.id, m.uid, m.internal_date, m.sent_date, m.subject, m.from_addr,
		       m.to_addrs, m.cc_addrs, m.message_id, m.in_reply_to, m.flags, m.raw_size,
		       (SELECT count(*) FROM attachments a WHERE a.message_id = m.id),
		       %s
		  FROM messages m
		 WHERE %s
		 ORDER BY m.uid
		 LIMIT $%d`, textCol, strings.Join(conds, " AND "), len(args))

	// A page that inlines text bodies is bounded by BYTES as well as rows
	// (RA6X-040): `limit` alone bounds the row count, and a message may be
	// 50 MiB. A page cut short here still hands back a cursor, so the client
	// simply pages more often — nothing is lost or reordered.
	maxBytes := int64(0)
	if fields.text {
		maxBytes = s.maxPageBytes()
	}
	items, truncated, err := s.scanMessageItemsBounded(r.Context(), query, args, maxBytes)
	if err != nil {
		slog.Error("folder messages query", "err", err)
		problemInternal(w, r)
		return
	}

	nextCursor := ""
	switch {
	case len(items) > limit:
		items = items[:limit]
		nextCursor = encodeCursor(items[len(items)-1].UID)
	case truncated && len(items) > 0:
		nextCursor = encodeCursor(items[len(items)-1].UID)
	}
	if fields.annotation {
		var annotationTruncated bool
		items, annotationTruncated, err = s.attachAnnotations(r.Context(), items)
		if err != nil {
			slog.Error("folder messages annotations", "err", err)
			problemInternal(w, r)
			return
		}
		if annotationTruncated {
			nextCursor = encodeCursor(items[len(items)-1].UID)
		}
	}

	resp := map[string]any{"mailbox": mailbox, "folder": folder, "messages": items}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	writeJSON(w, http.StatusOK, resp)
}

// addAnnotationFilters appends the annotation-sidecar filters:
//   - tag= / category= match on a stored label
//   - annotated_by=<model> / not_annotated_by=<model> match on whether a given
//     model has annotated the message (the per-model worker cooperation knob:
//     each GPU box exports not_annotated_by=<its own model> to pull only its
//     undone work)
//
// Writes the error response and returns false when the caller may not use a
// content-revealing filter, or when the sidecar table is not present yet.
//
// tag= and category= REQUIRE read_content (RA6X-017). Their values are
// body-derived labels an LLM wrote after reading the message, and although the
// response carries only metadata, the FILTER itself answers a content
// question: a metadata-only token could ask `?tag=sensitive-health` and read
// the answer off which messages came back, one guess at a time. That is the
// same inference /v1/search already refuses for the same reason — its results
// are metadata-shaped too, and it still demands read_content because the
// matching is content-shaped. Applying the rule in this helper rather than at
// each call site is deliberate: folder listing, search and export all reach
// the filters through here, and one of the three had already forgotten.
//
// annotated_by= and not_annotated_by= stay available to a metadata-only token.
// They answer "has this pipeline run over this message", not "what does this
// message say": the model name comes from the caller, the sidecar row's
// existence is operational state, and no label the LLM produced is exposed or
// probed. That is also what makes the worker cooperation knob work for a
// least-privilege token that only needs to know its own undone work.
func (s *server) addAnnotationFilters(w http.ResponseWriter, r *http.Request, tok *apiToken, conds *[]string, args *[]any) bool {
	tag := r.URL.Query().Get("tag")
	category := r.URL.Query().Get("category")
	annotatedBy := r.URL.Query().Get("annotated_by")
	notAnnotatedBy := r.URL.Query().Get("not_annotated_by")
	if tag == "" && category == "" && annotatedBy == "" && notAnnotatedBy == "" {
		return true
	}
	if tag != "" || category != "" {
		// Before the availability check, so an unauthorized caller learns
		// nothing about which migrations this deployment has applied.
		if !requirePermission(w, r, tok, auth.PermissionReadContent) {
			return false
		}
	}
	if !s.annotationsAvailable {
		problemUnavailable(w, r, "Annotation filters require the message_annotations migration in epistula-database.")
		return false
	}
	if tag != "" {
		*args = append(*args, tag)
		*conds = append(*conds, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM message_annotations an WHERE an.message_id = m.id AND $%d = ANY(an.tags))", len(*args)))
	}
	if category != "" {
		*args = append(*args, category)
		*conds = append(*conds, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM message_annotations an WHERE an.message_id = m.id AND an.category = $%d)", len(*args)))
	}
	if annotatedBy != "" {
		*args = append(*args, annotatedBy)
		*conds = append(*conds, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM message_annotations an WHERE an.message_id = m.id AND an.model = $%d)", len(*args)))
	}
	if notAnnotatedBy != "" {
		*args = append(*args, notAnnotatedBy)
		*conds = append(*conds, fmt.Sprintf(
			"NOT EXISTS (SELECT 1 FROM message_annotations an WHERE an.message_id = m.id AND an.model = $%d)", len(*args)))
	}
	return true
}

// scanMessageItems runs a message-page query whose SELECT list matches the
// canonical 14-column shape used by list, search, and export.
func (s *server) scanMessageItems(ctx context.Context, query string, args []any) ([]messageItem, error) {
	items, _, err := s.scanMessageItemsBounded(ctx, query, args, 0)
	return items, err
}

// scanMessageItemsBounded is scanMessageItems with an aggregate retained-BYTES
// budget, reporting whether it stopped early because the page was full
// (RA6X-040). A budget of 0 is unbounded, which is what a metadata-only page
// gets — its rows are small and fixed-shaped.
func (s *server) scanMessageItemsBounded(ctx context.Context, query string, args []any, maxBytes int64) ([]messageItem, bool, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var budget *pageBudget
	if maxBytes > 0 {
		budget = newPageBudget(maxBytes)
	}

	items := []messageItem{}
	for rows.Next() {
		var m messageItem
		if err := rows.Scan(&m.ID, &m.UID, &m.InternalDate, &m.SentDate, &m.Subject, &m.From,
			&m.To, &m.Cc, &m.MessageID, &m.InReplyTo, &m.Flags, &m.Size,
			&m.AttachmentCount, &m.TextBody); err != nil {
			return nil, false, err
		}
		if budget != nil && !budget.admit(itemBytes(&m)) {
			// Stop reading: the remaining rows belong to the next page, and
			// draining them here is exactly the memory this avoids.
			return items, true, nil
		}
		items = append(items, m)
	}
	return items, false, rows.Err()
}

// attachAnnotations decorates a page of items with their sidecar rows.
// attachAnnotations bounds whole message documents, preserving every sidecar.
// Load one message's annotations at a time so model multiplicity in later
// messages cannot defeat the page budget before it is checked. The first
// complete document is always admitted; a deferred document is cursor-paged.
func (s *server) attachAnnotations(ctx context.Context, items []messageItem) ([]messageItem, bool, error) {
	budget := newPageBudget(s.maxPageBytes())
	for i := range items {
		annots, err := s.loadAnnotations(ctx, []int64{items[i].ID})
		if err != nil {
			return nil, false, err
		}
		list := annots[items[i].ID]
		n := itemBytes(&items[i])
		for j := range list {
			n += annotationBytes(&list[j])
		}
		if !budget.admit(n) {
			clear(items[i:]) // release bodies backing the deferred tail
			return items[:i], true, nil
		}
		items[i].Annotations = list
	}
	return items, false, nil
}
