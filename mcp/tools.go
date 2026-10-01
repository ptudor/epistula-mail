// The seven MCP tools over the mail store. Six read (mailboxes, folders,
// messages, message, message_text, search) and one writes (annotate). Each tool
// turns into exactly one epistula-api /v1 request; the response is slimmed to the
// fields that answer the question, nulls/empties omitted, message text capped.
//
// Tool names and argument names are chosen for the model to reason about, not for
// parity with epistula-api's URL shapes. Input structs use `jsonschema` tags so the
// SDK generates the schema — we never hand-write JSON Schema.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolset struct {
	c *client // read client, holds cfg.Token
	// w is the write client for `annotate`, holding a SEPARATE epistula-api token.
	// nil when no annotate_token is configured, in which case the annotate tool
	// is never registered — see register(). Keeping the write credential in a
	// distinct client is what makes "the read path cannot write" a property of
	// the type rather than a convention.
	w   *client
	cfg config
}

// ---------------------------------------------------------------------------
// Result + slimming helpers. epistula-api payloads decode into `any`; the loosely
// typed accessors below read its objects and arrays.
// ---------------------------------------------------------------------------

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asSlice(v any) []any        { s, _ := v.([]any); return s }

// jsonResult wraps a JSON-serialisable value as the tool result text the model
// reads.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

// toolErr returns an error string as a normal (non-protocol-error) tool result,
// so a bad argument reads back to the model as data it can correct rather than a
// hard fault.
func toolErr(format string, a ...any) (*mcp.CallToolResult, any, error) {
	return jsonResult(map[string]any{"error": fmt.Sprintf(format, a...)})
}

// fail turns a bounded upstream/transport *apiError into a correctable
// `{"error": …}` tool result — the model sees status/title/detail and can
// correct (e.g. a 403 means the token isn't scoped for that mailbox/permission).
func fail(e *apiError) (*mcp.CallToolResult, any, error) {
	return jsonResult(map[string]any{"error": e})
}

// put sets params[key]=val only when val is non-empty, keeping absent filters
// out of the query string entirely.
func put(params url.Values, key, val string) {
	if val != "" {
		params.Set(key, val)
	}
}

// Nonpositive tool limits mean omitted. Ask the API to apply its configured
// default capped by this connector, in the same request. A deployment's default
// may be smaller than the shipped value; never replace it with a guessed 50.
func (t *toolset) clampLimit(limit int) string {
	if limit <= 0 {
		return "default:" + strconv.Itoa(t.cfg.MaxLimit)
	}
	if limit > t.cfg.MaxLimit {
		limit = t.cfg.MaxLimit
	}
	return strconv.Itoa(limit)
}

// normTime normalises a time argument for epistula-api's since/before/sent_since/
// sent_before. Absolute forms (RFC 3339, or a bare YYYY-MM-DD date) pass through
// unchanged; a relative lookback ("30m", "6h", "2d", "1w") is converted to an
// absolute RFC 3339 timestamp of now minus that duration, because epistula-api's
// parseTimeParam only understands absolute times. Empty stays empty.
func normTime(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s, nil // absolute — pass through
	}
	if _, err := time.Parse("2006-01-02", s); err == nil {
		return s, nil // bare date — epistula-api accepts it as UTC midnight
	}
	d, err := parseLookback(s)
	if err != nil {
		return "", err
	}
	return time.Now().Add(-d).UTC().Format(time.RFC3339), nil
}

// parseLookback turns a relative window into a positive duration. Accepts Go
// durations ("90m", "6h") plus the day/week suffixes time.ParseDuration doesn't
// know ("2d", "1w").
func parseLookback(s string) (time.Duration, error) {
	if n, unit, ok := splitNumUnit(s); ok {
		switch unit {
		case "d":
			return scaledLookback(s, n, float64(24*time.Hour))
		case "w":
			return scaledLookback(s, n, float64(7*24*time.Hour))
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad time value %q (use RFC 3339 like 2026-07-01T00:00:00Z, a date like 2026-07-01, or a lookback like 30m, 6h, 2d, 1w)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("time lookback must be positive, got %q", s)
	}
	return d, nil
}

// scaledLookback converts a day/week count to a duration under the SAME
// validation ordinary Go durations get (RA6X-058).
//
// The d/w branches used to convert a float product straight to time.Duration
// and return, skipping the positivity check below them entirely. `0d` and `0w`
// became a zero window — a search for "since now", which quietly matches
// nothing — and a fraction small enough to round to zero nanoseconds did the
// same. In the other direction, converting an out-of-range float64 to an
// integer type is implementation-defined in Go, and on this host `999999999d`
// landed on MaxInt64: a lookback of 292 years, presented to the model as if it
// had asked for one. An argument error is the only honest answer to any of
// those, and it costs no upstream request.
func scaledLookback(s string, n, scale float64) (time.Duration, error) {
	ns := n * scale
	if math.IsNaN(ns) || math.IsInf(ns, 0) {
		return 0, fmt.Errorf("time lookback %q is not a finite duration", s)
	}
	// float64(math.MaxInt64) rounds UP to 2^63, so >= is the correct guard:
	// any value at or past it is unrepresentable as a time.Duration.
	if ns >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("time lookback %q is too large (the maximum is about %s)", s,
			time.Duration(math.MaxInt64).Round(time.Hour))
	}
	d := time.Duration(ns)
	if d <= 0 {
		if ns > 0 {
			return 0, fmt.Errorf("time lookback %q is shorter than a nanosecond", s)
		}
		return 0, fmt.Errorf("time lookback must be positive, got %q", s)
	}
	return d, nil
}

// splitNumUnit parses a bare "<number><unit>" like "2d" → (2, "d", true).
func splitNumUnit(s string) (float64, string, bool) {
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 || i == len(s) {
		return 0, "", false
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", false
	}
	return n, s[i:], true
}

// escapePathData encodes one piece of DATA — a mailbox or folder name read out
// of the database — as exactly one URL path segment (RA6X-057).
//
// The names are arbitrary strings. IMAP accepts a folder literally called `.`,
// `..`, `A/../B` or `A//B`, and this connector used to lay a folder's own
// slashes into the URL as real path separators, escaping each piece with
// url.PathEscape. That leaves the name's structure at the mercy of routing:
// Go's ServeMux runs path.Clean over the request path and 301-redirects to the
// cleaned form, so `A/../B` arrived at the handler as `B` and `A//B` as `A/B`.
// The connector followed that redirect — it is same-origin, which RA6X-039's
// policy permits — and returned ANOTHER FOLDER'S MESSAGES with no sign that
// anything had been substituted.
//
// Two escapes matter here and url.PathEscape performs only one of them:
//
//   - "/" becomes %2F, which PathEscape does, collapsing the whole name into a
//     single segment so repeated and leading/trailing slashes survive;
//   - "." becomes %2E, which PathEscape deliberately does NOT, because a dot is
//     an unreserved character that is perfectly legal inside a segment. Path
//     cleaning does not care: it removes a segment that IS a dot. Escaping
//     every dot is the blunt version of that rule and needs no reasoning about
//     which dots are load-bearing.
//
// Cleaning runs over the ESCAPED path, so `%2E` and `%2F` are opaque to it, and
// the multi-segment wildcard unescapes its value exactly once — the handler
// receives the original bytes. The cost is a less readable URL in a log; the
// alternative is a connector that silently reads the wrong folder.
func escapePathData(s string) string {
	// url.PathEscape never emits a "." of its own (a percent-triplet is
	// "%" + two hex digits), so every dot left in its output came from s.
	return strings.ReplaceAll(url.PathEscape(s), ".", "%2E")
}

// setIf copies a present-and-non-nil value into the slim map under key.
func setStr(m map[string]any, key string, v any) {
	if s, ok := v.(string); ok && s != "" {
		m[key] = s
	}
}

// slimMessage projects one epistula-api message row to the fields a model needs.
// When capText is true the inline text_body (present only if the caller asked
// for it) is byte-capped and flagged. Nulls/empties are omitted.
func (t *toolset) slimMessage(row map[string]any) map[string]any {
	m := map[string]any{"id": row["id"]}
	if v, ok := row["uid"]; ok {
		m["uid"] = v
	}
	setStr(m, "mailbox", row["mailbox"])
	setStr(m, "folder", row["folder"])
	setStr(m, "subject", row["subject"])
	setStr(m, "from", row["from"])
	if to := asSlice(row["to"]); len(to) > 0 {
		m["to"] = to
	}
	if cc := asSlice(row["cc"]); len(cc) > 0 {
		m["cc"] = cc
	}
	setStr(m, "date", row["internal_date"])
	setStr(m, "sent_date", row["sent_date"])
	if fl := asSlice(row["flags"]); len(fl) > 0 {
		m["flags"] = fl
	}
	if v, ok := row["size"]; ok {
		m["size"] = v
	}
	if n, ok := row["attachment_count"]; ok {
		// Zero is omitted to keep the slimmed row terse. The value is a
		// json.Number now (RA6X-056), so the "is it zero" question is asked of
		// its decimal text rather than of a float64 the decoder no longer
		// produces — and the value itself is forwarded untouched, never
		// converted.
		if !isZeroNumber(n) {
			m["attachment_count"] = n
		}
	}
	return m
}

// isZeroNumber reports whether v is a JSON number equal to zero, without
// converting it to a float. A non-number is never zero for this purpose.
func isZeroNumber(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	i, err := n.Int64()
	return err == nil && i == 0
}

// ---------------------------------------------------------------------------
// mailboxes
// ---------------------------------------------------------------------------

const mailboxesDesc = "List the mailboxes this connector can see, with used/quota bytes and " +
	"whether each is disabled. RUN THIS FIRST to orient yourself: every other tool " +
	"addresses a mailbox by the `name` returned here. If a mailbox you expect is " +
	"missing, the epistula-api token simply isn't scoped to it."

type mailboxesInput struct{}

func (t *toolset) mailboxes(ctx context.Context, _ *mcp.CallToolRequest, _ mailboxesInput) (*mcp.CallToolResult, any, error) {
	data, aerr := t.c.getJSON(ctx, "/mailboxes", nil)
	if aerr != nil {
		return fail(aerr)
	}
	return jsonResult(map[string]any{"mailboxes": asSlice(asMap(data)["mailboxes"])})
}

// ---------------------------------------------------------------------------
// folders
// ---------------------------------------------------------------------------

const foldersDesc = "List the folders in one mailbox — name, uidvalidity, and message_count — " +
	"the store's shape and volume for that mailbox. Use `mailboxes` first to get the " +
	"mailbox name. A 403 means the token isn't scoped to that mailbox: ask about a " +
	"mailbox that `mailboxes` returned instead of retrying."

type foldersInput struct {
	Mailbox string `json:"mailbox" jsonschema:"mailbox name, exactly as returned by the mailboxes tool"`
}

func (t *toolset) folders(ctx context.Context, _ *mcp.CallToolRequest, in foldersInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Mailbox) == "" {
		return toolErr("mailbox is required (get one from the mailboxes tool)")
	}
	data, aerr := t.c.getJSON(ctx, "/mailboxes/"+escapePathData(in.Mailbox)+"/folders", nil)
	if aerr != nil {
		return fail(aerr)
	}
	m := asMap(data)
	out := []any{}
	for _, f := range asSlice(m["folders"]) {
		fm := asMap(f)
		row := map[string]any{"name": fm["name"], "message_count": fm["message_count"]}
		if v, ok := fm["uidvalidity"]; ok {
			row["uidvalidity"] = v
		}
		setStr(row, "special_use", fm["special_use"])
		out = append(out, row)
	}
	return jsonResult(map[string]any{"mailbox": m["mailbox"], "folders": out})
}

// ---------------------------------------------------------------------------
// messages — paginated list in one folder
// ---------------------------------------------------------------------------

const messagesDesc = "List messages in one folder, newest UIDs last, paginated. Metadata only " +
	"(no bodies — use `message` or `message_text` for a body). Filters: `since`/`before` " +
	"on the received date and `sent_since`/`sent_before` on the sent date (each takes an " +
	"absolute time like 2026-07-01 or 2026-07-01T00:00:00Z, or a lookback like 2d, 1w, 6h); " +
	"`flag`/`not_flag` (e.g. \\Seen, \\Flagged); `larger`/`smaller` in bytes; `tag` and " +
	"`category` from stored annotations; `annotated_by`/`not_annotated_by` by annotation " +
	"model. Page with `limit` (clamped) and by passing the returned `next_cursor` back as " +
	"`cursor` — cursors are opaque, never build one yourself."

type messagesInput struct {
	Mailbox        string `json:"mailbox" jsonschema:"mailbox name from the mailboxes tool"`
	Folder         string `json:"folder" jsonschema:"folder name from the folders tool (may contain slashes, e.g. Archive/2026)"`
	Since          string `json:"since,omitempty" jsonschema:"only messages received at/after this time (RFC 3339, a date, or a lookback like 2d)"`
	Before         string `json:"before,omitempty" jsonschema:"only messages received before this time (RFC 3339, a date, or a lookback like 2d)"`
	SentSince      string `json:"sent_since,omitempty" jsonschema:"only messages with a sent date at/after this time"`
	SentBefore     string `json:"sent_before,omitempty" jsonschema:"only messages with a sent date before this time"`
	Flag           string `json:"flag,omitempty" jsonschema:"only messages carrying this IMAP flag, e.g. \\Seen or \\Flagged"`
	NotFlag        string `json:"not_flag,omitempty" jsonschema:"only messages NOT carrying this IMAP flag"`
	Larger         int    `json:"larger,omitempty" jsonschema:"only messages larger than this many bytes"`
	Smaller        int    `json:"smaller,omitempty" jsonschema:"only messages smaller than this many bytes"`
	Tag            string `json:"tag,omitempty" jsonschema:"only messages carrying this annotation tag"`
	Category       string `json:"category,omitempty" jsonschema:"only messages with this stored annotation category"`
	AnnotatedBy    string `json:"annotated_by,omitempty" jsonschema:"only messages annotated by this model"`
	NotAnnotatedBy string `json:"not_annotated_by,omitempty" jsonschema:"only messages NOT annotated by this model"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"opaque pagination cursor from a previous next_cursor; do not fabricate"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max messages to return this page (clamped to the server max)"`
}

func (t *toolset) messages(ctx context.Context, _ *mcp.CallToolRequest, in messagesInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Mailbox) == "" || strings.TrimSpace(in.Folder) == "" {
		return toolErr("both mailbox and folder are required (get them from the mailboxes and folders tools)")
	}
	params := url.Values{}
	for _, tp := range []struct{ key, val string }{
		{"since", in.Since}, {"before", in.Before},
		{"sent_since", in.SentSince}, {"sent_before", in.SentBefore},
	} {
		norm, err := normTime(tp.val)
		if err != nil {
			return toolErr("%s: %v", tp.key, err)
		}
		put(params, tp.key, norm)
	}
	put(params, "flag", in.Flag)
	put(params, "not_flag", in.NotFlag)
	if in.Larger > 0 {
		params.Set("larger", strconv.Itoa(in.Larger))
	}
	if in.Smaller > 0 {
		params.Set("smaller", strconv.Itoa(in.Smaller))
	}
	put(params, "tag", in.Tag)
	put(params, "category", in.Category)
	put(params, "annotated_by", in.AnnotatedBy)
	put(params, "not_annotated_by", in.NotAnnotatedBy)
	put(params, "cursor", in.Cursor)
	put(params, "limit", t.clampLimit(in.Limit))

	path := "/mailboxes/" + escapePathData(in.Mailbox) + "/folders/" + escapePathData(in.Folder) + "/messages"
	data, aerr := t.c.getJSON(ctx, path, params)
	if aerr != nil {
		return fail(aerr)
	}
	// The response says which folder epistula-api actually read. Check it against
	// the folder we asked for (RA6X-057).
	//
	// escapePathData makes epistula-api's own router unable to substitute a
	// different folder, but it is not the only thing between the two
	// processes: a reverse proxy in front of the API can normalise a path too,
	// and RA6X-039's redirect policy stays as it is — a same-origin redirect is
	// followed, because a proxy canonicalising an approved base path is a real
	// deployment and refusing every redirect would break it. What must not
	// happen is that a substitution goes unnoticed, so the echo is compared
	// rather than the redirect being banned.
	if err := checkFolderEcho(data, in.Folder); err != nil {
		return toolErr("%v", err)
	}
	return t.messageListResult(data)
}

// checkFolderEcho verifies that the folder epistula-api reports having read is the
// folder that was requested. epistula-api echoes the path it resolved in every
// folder-messages response, so a mismatch means something between here and the
// database rewrote the request — the answer is real mail, but from the wrong
// folder, which is worse than an error.
func checkFolderEcho(data any, want string) error {
	got, ok := asMap(data)["folder"].(string)
	if !ok {
		return fmt.Errorf("epistula-api did not report which folder it read; refusing to " +
			"present the result as folder contents")
	}
	if got != want {
		return fmt.Errorf("epistula-api read folder %q, not the requested %q; the request path "+
			"was rewritten in transit", got, want)
	}
	return nil
}

// messageListResult slims the shared {messages, next_cursor} list shape used by
// the messages and search tools.
func (t *toolset) messageListResult(data any) (*mcp.CallToolResult, any, error) {
	m := asMap(data)
	out := []any{}
	for _, row := range asSlice(m["messages"]) {
		out = append(out, t.slimMessage(asMap(row)))
	}
	resp := map[string]any{"messages": out}
	setStr(resp, "mailbox", m["mailbox"])
	setStr(resp, "folder", m["folder"])
	setStr(resp, "q", m["q"])
	setStr(resp, "next_cursor", m["next_cursor"])
	return jsonResult(resp)
}

// ---------------------------------------------------------------------------
// message — one message in full
// ---------------------------------------------------------------------------

const messageDesc = "Use next_attachment/next_annotation as the corresponding after fields to continue metadata; use a model and its summary_next_offset to continue a long summary. Fetch one message by numeric id: subject, from/to/cc, dates, flags, " +
	"attachment metadata, and its ranked annotations (tags, category, summary — highest-" +
	"priority model first, the preferred one flagged primary). The plaintext body is " +
	"included but capped; the HTML body is never returned. For a long body use " +
	"`message_text` to page through it. Get an id from `messages` or `search`."

type messageInput struct {
	AttachmentAfter string `json:"attachment_after,omitempty" jsonschema:"next_attachment from the preceding metadata page"`
	AnnotationAfter string `json:"annotation_after,omitempty" jsonschema:"next_annotation from the preceding metadata page"`
	AnnotationModel string `json:"annotation_model,omitempty" jsonschema:"one returned model whose summary should be continued"`
	SummaryOffset   int64  `json:"summary_offset,omitempty" jsonschema:"summary_next_offset from that model, in UTF-8 bytes"`
	ID              int64  `json:"id" jsonschema:"numeric message id from the messages or search tool"`
}

func (t *toolset) message(ctx context.Context, _ *mcp.CallToolRequest, in messageInput) (*mcp.CallToolResult, any, error) {
	if in.ID <= 0 {
		return toolErr("id must be a positive message id (get one from messages or search)")
	}
	// Ask epistula-api to bound the document at the source (RA6X-046).
	//
	// The full document inlines BOTH bodies, so a message with more than the
	// connector's response budget of serialized text and HTML failed this tool
	// outright — even when the caller only wanted its metadata, its attachment
	// list or a short preview, and even when the useful plaintext was a few
	// lines. The client rejects an over-budget response before capText ever
	// runs, so capping locally could not help: the bytes had to not be sent.
	//
	// html=false drops a body this tool never forwards anyway, and text_limit
	// asks for exactly the preview that will survive the cap. Both parameters
	// are additive and absent by default in epistula-api, so no other consumer is
	// affected, and the paged message_text tool still returns the body in full.
	params := url.Values{}
	params.Set("html", "false")
	params.Set("metadata", "inspection")
	put(params, "attachment_after", in.AttachmentAfter)
	put(params, "annotation_after", in.AnnotationAfter)
	put(params, "annotation_model", in.AnnotationModel)
	if in.SummaryOffset < 0 || (in.SummaryOffset > 0 && in.AnnotationModel == "") {
		return toolErr("summary_offset must be non-negative and a positive offset requires annotation_model")
	}
	if in.SummaryOffset > 0 {
		params.Set("summary_offset", strconv.FormatInt(in.SummaryOffset, 10))
	}
	params.Set("text_limit", strconv.Itoa(t.cfg.MaxTextBytes))
	data, aerr := t.c.getJSON(ctx, "/messages/"+strconv.FormatInt(in.ID, 10), params)
	if aerr != nil {
		return fail(aerr)
	}
	row := asMap(data)
	out := t.slimMessage(row)
	setStr(out, "message_id", row["message_id"])
	setStr(out, "in_reply_to", row["in_reply_to"])

	// Cap the inline plaintext; never forward html_body. The server has
	// normally already bounded it, but an older epistula-api that does not know
	// text_limit sends the whole body, and this tool's own contract is the cap.
	if body, ok := row["text_body"].(string); ok && body != "" {
		text, truncated := capText(body, 0, t.cfg.MaxTextBytes)
		out["text_body"] = text
		if truncated || row["text_truncated"] == true {
			out["text_body_truncated"] = true
		}
	}
	delete(out, "html_body")
	out["attachments"] = slimAttachments(asSlice(row["attachments"]))
	for _, key := range []string{"next_attachment", "next_annotation"} {
		if value, ok := row[key].(string); ok && value != "" {
			out[key] = value
			out["metadata_truncated"] = true
		}
	}
	if an := asSlice(row["annotations"]); len(an) > 0 {
		out["annotations"] = slimAnnotations(an)
	}
	return jsonResult(out)
}

// slimAttachments projects attachment metadata to filename/content_type/size
// (per-part), dropping the sha256 and structural detail a model doesn't need.
func slimAttachments(atts []any) []any {
	out := []any{}
	for _, a := range atts {
		am := asMap(a)
		row := map[string]any{"content_type": am["content_type"], "size_bytes": am["size_bytes"]}
		setStr(row, "filename", am["filename"])
		setStr(row, "part_number", am["part_number"])
		setStr(row, "disposition", am["disposition"])
		out = append(out, row)
	}
	return out
}

// slimAnnotations keeps the ranking-relevant fields and the prose, dropping the
// token accounting a model doesn't need.
func slimAnnotations(anns []any) []any {
	out := []any{}
	for _, a := range anns {
		am := asMap(a)
		row := map[string]any{"model": am["model"]}
		if tags := asSlice(am["tags"]); len(tags) > 0 {
			row["tags"] = tags
		}
		setStr(row, "category", am["category"])
		setStr(row, "summary", am["summary"])
		for _, key := range []string{"summary_bytes", "summary_offset", "summary_next_offset"} {
			if value, ok := am[key]; ok {
				row[key] = value
			}
		}
		setStr(row, "created_at", am["created_at"])
		if v, ok := am["priority"]; ok {
			row["priority"] = v
		}
		if p, ok := am["primary"].(bool); ok && p {
			row["primary"] = true
		}
		out = append(out, row)
	}
	return out
}

// ---------------------------------------------------------------------------
// message_text — plaintext body, byte-capped, resumable
// ---------------------------------------------------------------------------

const messageTextDesc = "Fetch just the ready-to-read plaintext body of one message (HTML is " +
	"already converted to text at ingest). The body is returned starting at `offset` bytes " +
	"and capped; if `truncated` is true, call again with `next_offset` to continue. Use " +
	"this for a long body that `message` truncated. Get an id from `messages` or `search`."

type messageTextInput struct {
	ID     int64 `json:"id" jsonschema:"numeric message id from the messages or search tool"`
	Offset int   `json:"offset,omitempty" jsonschema:"byte offset to start from (use next_offset from a prior truncated call); default 0"`
}

func (t *toolset) messageText(ctx context.Context, _ *mcp.CallToolRequest, in messageTextInput) (*mcp.CallToolResult, any, error) {
	if in.ID <= 0 {
		return toolErr("id must be a positive message id (get one from messages or search)")
	}
	if in.Offset < 0 {
		return toolErr("offset must be non-negative")
	}
	// Ask epistula-api for just this page. Fetching the whole body per call and
	// slicing locally made walking a large body O(n^2) in both bytes
	// transferred and bytes allocated, while the next_offset protocol
	// advertised to the model implied incremental fetching (RO5X-016).
	//
	// Request one byte past the page so a full page is distinguishable from
	// a body that happens to end exactly on the boundary.
	page, aerr := t.c.getTextRange(
		ctx, "/messages/"+strconv.FormatInt(in.ID, 10)+"/text",
		in.Offset, t.cfg.MaxTextBytes+1)
	if aerr != nil {
		return fail(aerr)
	}

	var (
		text      string
		truncated bool
		// start is the byte the returned text ACTUALLY begins at, which is not
		// necessarily the offset that was asked for: an offset landing inside a
		// multibyte rune is advanced to the next boundary (RA6X-018). The
		// response reports this value and next_offset is computed from it, so a
		// client that echoes the cursor forward always advances.
		start = page.Start
		total = page.Total
	)
	if page.HasTotal {
		// Server-side projection: page.Text already starts at page.Start.
		text, truncated = capText(page.Text, 0, t.cfg.MaxTextBytes)
		// A page cut short by the server (rune back-off) still leaves
		// bytes behind whenever we have not reached the end.
		if !truncated && start+len(text) < total {
			truncated = true
		}
	} else {
		// Older epistula-api with no ?offset=/?limit= support: it sent the whole
		// body, so slice locally — under the SAME rune rules the server
		// applies, including normalizing the start (RA6X-018). Slicing at an
		// interior byte here used to be possible, and the two paths could
		// disagree about where a page began.
		total = len(page.Text)
		start = runeStartAt(page.Text, in.Offset)
		text, truncated = capText(page.Text, start, t.cfg.MaxTextBytes)
	}

	out := map[string]any{
		"id":          in.ID,
		"text":        text,
		"offset":      start,
		"total_bytes": total,
	}
	if truncated {
		out["truncated"] = true
		next := start + len(text)
		if next <= start && start < total {
			// Defence in depth: a cursor that does not move is a client loop.
			// capText guarantees a non-empty page whenever bytes remain, so
			// this cannot fire — but if it ever did, an error the model can
			// report beats a tool it can call forever.
			return toolErr("message text paging made no progress at offset %d; "+
				"the configured max_text_bytes may be too small", start)
		}
		out["next_offset"] = next
	}
	return jsonResult(out)
}

// runeStartAt normalizes a byte offset forward to the start of a rune, matching
// what epistula-api's own projection does (RA6X-018).
func runeStartAt(s string, offset int) int {
	if offset < 0 {
		return 0
	}
	if offset >= len(s) {
		return len(s)
	}
	for offset < len(s) && !utf8.RuneStart(s[offset]) {
		offset++
	}
	return offset
}

// capText returns up to maxBytes of s starting at offset, trimmed back to a UTF-8
// rune boundary so a multibyte character is never split. truncated reports
// whether bytes remain after the returned slice. An offset past the end yields an
// empty, non-truncated result.
func capText(s string, offset, maxBytes int) (string, bool) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(s) {
		return "", false
	}
	end := len(s)
	truncated := false
	if maxBytes > 0 && len(s)-offset > maxBytes {
		end = offset + maxBytes
		truncated = true
		// Back off to the start of the rune straddling the cut, if any.
		for end > offset && !utf8.RuneStart(s[end]) {
			end--
		}
		if end == offset {
			// The next rune is wider than the whole budget. Returning an empty
			// page with truncated=true is what made next_offset equal the
			// input offset forever, so a model paging a body looped without
			// ever moving (RA6X-018). One COMPLETE rune goes out instead,
			// overshooting the budget by at most three bytes: a bounded
			// overshoot beats an unbounded loop, and a rune is the smallest
			// unit that can be handed out without splitting a character. The
			// configuration floor makes this unreachable for a loaded config;
			// it is here so a caller that passes its own budget cannot
			// reintroduce the loop.
			_, size := utf8.DecodeRuneInString(s[offset:])
			end = offset + size
		}
	}
	return s[offset:end], truncated
}

// ---------------------------------------------------------------------------
// search — full-text over the fts tsvector
// ---------------------------------------------------------------------------

const searchDesc = "Full-text search message subjects and bodies (q= over the server's search " +
	"index). This is the free-text entry point when you don't know which folder to look in. " +
	"Optional filters: `mailbox` (and then `folder`), `tag`, `category`, and `since`/`before` " +
	"on the received date (absolute time, a date, or a lookback like 2d). Results are metadata " +
	"only, newest first; page with `limit` and the returned `next_cursor`. A 403 means the " +
	"token lacks read_content or isn't scoped to the requested mailbox."

type searchInput struct {
	Q        string `json:"q" jsonschema:"search text; matched against message subjects and bodies"`
	Mailbox  string `json:"mailbox,omitempty" jsonschema:"restrict to this mailbox (name from the mailboxes tool)"`
	Folder   string `json:"folder,omitempty" jsonschema:"restrict to this folder; requires mailbox"`
	Tag      string `json:"tag,omitempty" jsonschema:"restrict to messages carrying this annotation tag"`
	Category string `json:"category,omitempty" jsonschema:"restrict to messages with this annotation category"`
	Since    string `json:"since,omitempty" jsonschema:"only messages received at/after this time (RFC 3339, a date, or a lookback like 2d)"`
	Before   string `json:"before,omitempty" jsonschema:"only messages received before this time (RFC 3339, a date, or a lookback like 2d)"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"opaque pagination cursor from a previous next_cursor; do not fabricate"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max messages to return this page (clamped to the server max)"`
}

func (t *toolset) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Q) == "" {
		return toolErr("q is required (the text to search for)")
	}
	if strings.TrimSpace(in.Folder) != "" && strings.TrimSpace(in.Mailbox) == "" {
		return toolErr("folder requires mailbox")
	}
	params := url.Values{"q": {in.Q}}
	put(params, "mailbox", in.Mailbox)
	put(params, "folder", in.Folder)
	put(params, "tag", in.Tag)
	put(params, "category", in.Category)
	for _, tp := range []struct{ key, val string }{{"since", in.Since}, {"before", in.Before}} {
		norm, err := normTime(tp.val)
		if err != nil {
			return toolErr("%s: %v", tp.key, err)
		}
		put(params, tp.key, norm)
	}
	put(params, "cursor", in.Cursor)
	put(params, "limit", t.clampLimit(in.Limit))

	data, aerr := t.c.getJSON(ctx, "/search", params)
	if aerr != nil {
		return fail(aerr)
	}
	return t.messageListResult(data)
}

// ---------------------------------------------------------------------------
// annotate — the one write path
// ---------------------------------------------------------------------------

const annotateDesc = "Store (or replace) this model's annotation for one message: `tags`, an " +
	"advisory `category`, and an optional `summary`. Idempotent on (message id, model) — " +
	"re-annotating the same message with the same `model` replaces the previous annotation " +
	"rather than duplicating it. Requires the epistula-api token to carry write_annotation; a 403 " +
	"means it doesn't, and the write can't be done with this token. Provide `model` and at " +
	"least one of tags/category/summary."

type annotateInput struct {
	ID       int64    `json:"id" jsonschema:"numeric message id to annotate"`
	Model    string   `json:"model" jsonschema:"identifier for the annotating model, e.g. claude-opus-4-8; annotations are keyed (message id, model)"`
	Tags     []string `json:"tags,omitempty" jsonschema:"label set for the message, e.g. [receipts, hosting]"`
	Category string   `json:"category,omitempty" jsonschema:"advisory sort category (a suggested folder/label; the store records it but never moves the message)"`
	Summary  string   `json:"summary,omitempty" jsonschema:"optional prose summary of the message"`
}

func (t *toolset) annotate(ctx context.Context, _ *mcp.CallToolRequest, in annotateInput) (*mcp.CallToolResult, any, error) {
	// Unreachable while register() gates on t.w — kept because the cost is three
	// lines and the failure it prevents is a nil dereference in the one code
	// path that writes, reached from attacker-influenced input. A later change
	// that registers the tool unconditionally should get a clean tool error,
	// not a panic that takes the server down.
	if t.w == nil {
		return toolErr("annotate is not available: this connector was started without an " +
			"annotate_token, so it holds no credential that can write")
	}
	if in.ID <= 0 {
		return toolErr("id must be a positive message id (get one from messages or search)")
	}
	if strings.TrimSpace(in.Model) == "" {
		return toolErr("model is required (the identifier of the annotating model)")
	}
	if len(in.Tags) == 0 && strings.TrimSpace(in.Category) == "" && strings.TrimSpace(in.Summary) == "" {
		return toolErr("provide at least one of tags, category, or summary")
	}

	body := map[string]any{"model": strings.TrimSpace(in.Model)}
	if len(in.Tags) > 0 {
		body["tags"] = in.Tags
	}
	if s := strings.TrimSpace(in.Category); s != "" {
		body["category"] = s
	}
	if s := strings.TrimSpace(in.Summary); s != "" {
		body["summary"] = s
	}

	// t.w, not t.c: the write rides the annotate token. The read client's token
	// should not carry write_annotation at all.
	if aerr := t.w.put(ctx, "/messages/"+strconv.FormatInt(in.ID, 10)+"/annotation", body); aerr != nil {
		return fail(aerr)
	}
	return jsonResult(map[string]any{"ok": true, "id": in.ID, "model": strings.TrimSpace(in.Model)})
}

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

// register wires the tools onto the server.
//
// The six read tools are always registered. `annotate` is registered ONLY when a
// separate annotate_token is configured, because that credential is the tool's
// entire capability — with no token there is nothing for it to authenticate as.
//
// This is still "capability follows the token", not an MCP-side permission knob:
// there is no boolean saying whether writing is allowed, and this process does
// not decide what a token may do. What changed is that there are now two tokens,
// and the tool surface follows from which credentials exist. Mint the read token
// without write_annotation and epistula-api enforces the split in SQL as well.
//
// The reason to drop the tool rather than let an unscoped call earn a 403: every
// message body is attacker-controlled input, so a model reading mail can be told
// to write. A 403 stops the write but the tool is still reachable, and the
// attempt still happens. An unregistered tool cannot be called at all.
func (t *toolset) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "mailboxes", Description: mailboxesDesc}, t.mailboxes)
	mcp.AddTool(server, &mcp.Tool{Name: "folders", Description: foldersDesc}, t.folders)
	mcp.AddTool(server, &mcp.Tool{Name: "messages", Description: messagesDesc}, t.messages)
	mcp.AddTool(server, &mcp.Tool{Name: "message", Description: messageDesc}, t.message)
	mcp.AddTool(server, &mcp.Tool{Name: "message_text", Description: messageTextDesc}, t.messageText)
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: searchDesc}, t.search)
	if t.w != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "annotate", Description: annotateDesc}, t.annotate)
	}
}
