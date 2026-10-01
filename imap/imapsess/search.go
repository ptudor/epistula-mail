package imapsess

import (
	"fmt"
	"net/textproto"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/ptudor/epistula-mail/database/imapflags"
)

// Search executes the IMAP SEARCH command against the selected folder.
// The result NumSet returned in SearchData.All is a UIDSet when kind is
// NumKindUID, a SeqSet when NumKindSeq.
//
// Implementation strategy: walk the SearchCriteria tree and translate
// each leaf into a parameterized SQL fragment. Results come back as a
// list of (seqnum, uid) pairs from a single round-trip, then the right
// column gets packaged into the NumSet. Sequence numbers are 1-based
// positions over (folder_id, ORDER BY uid).
func (s *Session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (searchData *imap.SearchData, err error) {
	defer s.guard("SEARCH", &err)
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	if s.selectedFolderID == 0 {
		return nil, responseBadState("SEARCH: no mailbox selected")
	}
	if criteria == nil {
		criteria = &imap.SearchCriteria{}
	}

	ctx, cancel := s.queryCtx()
	defer cancel()
	if err := s.ensureView(ctx); err != nil {
		return nil, err
	}
	b := &searchSQL{view: s.view}
	b.args = append(b.args, s.selectedFolderID)
	whereExpr, err := b.buildCriteria(criteria)
	if err != nil {
		s.be.Logger.Warn("SEARCH build SQL", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "SEARCH: " + err.Error()}
	}
	if whereExpr == "" {
		whereExpr = "TRUE"
	}

	// Only wrap the scan in a row_number() window when sequence numbers are
	// actually needed — either as the result kind or as a SeqNum criterion. The
	// window forces Postgres to read and order every folder row before the
	// outer WHERE runs, so the fts GIN index (and b-trees) can't be used. When
	// the result is UID-kind and no SeqNum criterion appears, filter the base
	// table directly so those indexes are usable (R-041).
	useWindow := false
	whereExpr = "(" + whereExpr + ") AND uid <= " + b.placeholder(int64(s.view.maxUID()))

	// Bound the result set. Fetch max+1 so overflow is detectable.
	maxResults := s.be.MaxSearchResults
	limitClause := ""
	if maxResults > 0 {
		b.args = append(b.args, maxResults+1)
		limitClause = fmt.Sprintf(" LIMIT $%d", len(b.args))
	}
	query := searchQuery(useWindow, whereExpr, limitClause)

	rows, err := s.be.Pool.Query(ctx, query, b.args...)
	if err != nil {
		s.be.Logger.Error("SEARCH query", "err", err, "where", whereExpr)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SEARCH query failed"}
	}
	defer rows.Close()

	prealloc := 4096
	if maxResults > 0 && maxResults < prealloc {
		prealloc = maxResults
	}
	seqs := make([]uint32, 0, prealloc)
	uids := make([]imap.UID, 0, prealloc)
	var minVal, maxVal uint32
	var count uint32
	for rows.Next() {
		var seq, uid int64
		if err := rows.Scan(&seq, &uid); err != nil {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SEARCH scan failed"}
		}
		seq = int64(s.view.seqOf(uid))
		if seq == 0 {
			continue
		}
		seqs = append(seqs, uint32(seq))
		uids = append(uids, imap.UID(uid))
		count++
		switch kind {
		case imapserver.NumKindUID:
			if minVal == 0 || uint32(uid) < minVal {
				minVal = uint32(uid)
			}
			if uint32(uid) > maxVal {
				maxVal = uint32(uid)
			}
		default:
			if minVal == 0 || uint32(seq) < minVal {
				minVal = uint32(seq)
			}
			if uint32(seq) > maxVal {
				maxVal = uint32(seq)
			}
		}
	}
	if err := rows.Err(); err != nil {
		s.be.Logger.Error("SEARCH rows", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SEARCH query failed"}
	}

	// The query asked for max+1 rows; getting them all means the real result
	// set is larger than the cap. Fail loudly rather than truncating — a
	// client handed a short list would treat the missing UIDs as deleted.
	if maxResults > 0 && len(uids) > maxResults {
		s.be.Logger.Warn("SEARCH result set exceeded the configured cap",
			"mailbox", s.mailboxName, "folder", s.selectedFolderName, "max", maxResults)
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeLimit,
			Text: "SEARCH result set too large; narrow the criteria",
		}
	}
	if err := rows.Err(); err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SEARCH rows error"}
	}

	var all imap.NumSet
	switch kind {
	case imapserver.NumKindUID:
		var us imap.UIDSet
		us.AddNum(uids...)
		all = us
	default:
		var ss imap.SeqSet
		ss.AddNum(seqs...)
		all = ss
	}
	return &imap.SearchData{
		All:   all,
		Min:   minVal,
		Max:   maxVal,
		Count: count,
	}, nil
}

// searchQuery returns the SEARCH SQL. With useWindow, a row_number() window
// computes seqnum over the full folder ordering (correct positions, but the
// outer WHERE can't push into indexes). Without it (UID-kind, no SeqNum
// criterion), the criteria filter the base table directly so the fts GIN
// index and b-trees are usable; seqnum is unused for UID results, so a 0
// placeholder keeps the two-column scan shape (R-041).
// limitClause is appended by the caller when a result cap is configured; the
// placeholder index is one past the criteria args. The cap is fetched as
// max+1 rows so an overflow is detectable rather than silently truncating —
// a truncated SEARCH is worse than an error, since the client would treat
// the missing UIDs as deleted (RO5X-020).
func searchQuery(useWindow bool, whereExpr, limitClause string) string {
	if !useWindow {
		return fmt.Sprintf(`
			SELECT 0::bigint AS seqnum, uid
			  FROM messages
			 WHERE folder_id = $1 AND (%s)
			 ORDER BY uid%s`, whereExpr, limitClause)
	}
	// messages.* rather than a hand-maintained column list: the outer WHERE is
	// generated from arbitrary search criteria, so every column a criterion can
	// name has to survive into `m`. A fixed list silently loses that property
	// the moment a criterion learns a new column — which is how BODY over
	// text_body/html_body (RA6X-026) and SENTSINCE over sent_date_local
	// (RA6X-048) became `column does not exist`, on the Seq-kind path only.
	// Widening the projection costs nothing: Postgres does not detoast a large
	// column the query never reads.
	return fmt.Sprintf(`
		SELECT seqnum, uid FROM (
			SELECT row_number() OVER (ORDER BY uid)::bigint AS seqnum,
			       messages.*
			  FROM messages
			 WHERE folder_id = $1
		) m
		 WHERE %s
		 ORDER BY uid%s`, whereExpr, limitClause)
}

// searchSQL accumulates a parameterized WHERE expression and its args.
// All placeholders are $-numbered; args grows monotonically as fragments
// are emitted. usesSeqNum records whether any SeqNum criterion referenced the
// synthesized seqnum column, which forces the row_number() window.
type searchSQL struct {
	view       *sessionView
	args       []any
	usesSeqNum bool
}

// placeholder appends v to args and returns its $N placeholder string.
func (b *searchSQL) placeholder(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// buildCriteria walks a SearchCriteria and returns the SQL fragment
// representing it. An empty fragment means "match everything"; the
// caller substitutes TRUE in that case.
func (b *searchSQL) buildCriteria(c *imap.SearchCriteria) (string, error) {
	var clauses []string

	// SeqNum / UID restrictions are themselves disjunctions across the
	// passed sets (per RFC 9051 they AND with each other AND'd, but
	// each individual set is OR'd internally — same as message-number
	// list semantics). For typical clients only one of each appears.
	for _, ss := range c.SeqNum {
		clauses = append(clauses, b.seqSetClause(ss))
	}
	for _, us := range c.UID {
		clauses = append(clauses, b.uidSetClause(us))
	}

	// Date filters: SINCE/BEFORE on internal_date; SENTSINCE/SENTBEFORE
	// on sent_date. RFC 9051 says only the date portion is used —
	// truncate the values to dates at the SQL level.
	// Every timestamptz is reduced to a calendar date IN UTC, not in whatever
	// timezone the connection happens to carry (RA6X-048).
	//
	// `internal_date::date` uses the session TimeZone, so the same SEARCH
	// against the same data returned different results depending on a
	// connection setting nothing in this daemon sets — and disagreed with the
	// date the client is shown, since INTERNALDATE is rendered from the UTC
	// value. `AT TIME ZONE 'UTC'` makes the reduction explicit and stable.
	//
	// The client's boundary is reduced the same way, so both sides of the
	// comparison mean the same thing.
	if !c.Since.IsZero() {
		ph := b.placeholder(c.Since.UTC())
		clauses = append(clauses, fmt.Sprintf("(internal_date AT TIME ZONE 'UTC')::date >= (%s AT TIME ZONE 'UTC')::date", ph))
	}
	if !c.Before.IsZero() {
		ph := b.placeholder(c.Before.UTC())
		clauses = append(clauses, fmt.Sprintf("(internal_date AT TIME ZONE 'UTC')::date < (%s AT TIME ZONE 'UTC')::date", ph))
	}
	// SENTSINCE/SENTBEFORE compare against the sender's OWN calendar date —
	// the date written in the Date header, disregarding its time and zone
	// (RFC 9051 §6.4.4). sent_date is a timestamptz whose original offset is
	// gone, so sent_date_local (migration 017) carries that calendar date
	// directly; rows written before it fall back to the UTC reduction, which
	// is what the old comparison meant on a UTC connection.
	if !c.SentSince.IsZero() {
		ph := b.placeholder(c.SentSince.UTC())
		clauses = append(clauses, fmt.Sprintf(
			"COALESCE(sent_date_local, (sent_date AT TIME ZONE 'UTC')::date) >= (%s AT TIME ZONE 'UTC')::date", ph))
	}
	if !c.SentBefore.IsZero() {
		ph := b.placeholder(c.SentBefore.UTC())
		clauses = append(clauses, fmt.Sprintf(
			"COALESCE(sent_date_local, (sent_date AT TIME ZONE 'UTC')::date) < (%s AT TIME ZONE 'UTC')::date", ph))
	}

	// Size filters.
	if c.Larger > 0 {
		clauses = append(clauses, fmt.Sprintf("raw_size > %s", b.placeholder(c.Larger)))
	}
	if c.Smaller > 0 {
		clauses = append(clauses, fmt.Sprintf("raw_size < %s", b.placeholder(c.Smaller)))
	}

	// Flag containment. NotFlag negates.
	//
	// Canonicalize on the READ side too. R-063 canonicalized at the write
	// seam so byte-exact comparisons downstream would agree, but a row
	// written before that fix — or by any other writer, e.g. a psql fix-up
	// or an external administration tool — can still hold `\SEEN`. Against
	// such a row `SEARCH SEEN` (which go-imap normalizes to `\Seen`) matched
	// nothing and the message showed as unread forever.
	//
	// Keyword flags fold too (RA6X-028): flag names are case-insensitive, so
	// `SEARCH KEYWORD $junk` must find a stored `$Junk`. Because the write seam
	// stores one spelling per flag, canonicalizing the SEARCH term is enough —
	// the array membership test stays byte-exact.
	for _, f := range c.Flag {
		clauses = append(clauses, fmt.Sprintf("%s = ANY(flags)", b.placeholder(imapflags.Canonical(string(f)))))
	}
	for _, f := range c.NotFlag {
		clauses = append(clauses, fmt.Sprintf("NOT (%s = ANY(flags))", b.placeholder(imapflags.Canonical(string(f)))))
	}

	// Header field match: case-insensitive substring on values stored
	// under the canonical header name in the JSONB.
	for _, h := range c.Header {
		canonical := textproto.CanonicalMIMEHeaderKey(h.Key)
		if h.Value == "" {
			// IMAP HEADER name "" = "header exists".
			clauses = append(clauses,
				fmt.Sprintf("headers ? %s", b.placeholder(canonical)))
			continue
		}
		keyPh := b.placeholder(canonical)
		valPh := b.placeholder("%" + escapeLike(strings.ToLower(h.Value)) + "%")
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM jsonb_array_elements_text(headers -> %s) v WHERE LOWER(v) LIKE %s ESCAPE '\')`,
			keyPh, valPh))
	}

	// BODY and TEXT are SUBSTRING matches over different scopes (RA6X-026,
	// RFC 9051 §6.4.4).
	//
	// Both compiled to the same `fts` tsvector, which indexes subject plus
	// extracted plain text. That was wrong in three directions at once:
	//
	//   - BODY matched a word present only in the Subject, because the vector
	//     includes it — a false positive on a search of the body;
	//   - TEXT could not find a string that appears only in a header, because
	//     the vector excludes headers — a false negative on a search of the
	//     whole message;
	//   - token matching is not substring matching, so `BODY "oba"` missed a
	//     body containing "foobar", and punctuation or an empty string
	//     behaved arbitrarily.
	//
	// The predicates below are the authority: BODY over the body projections,
	// TEXT over the headers as well. FTS is retained only as a NARROWING
	// pre-filter where it provably cannot exclude a valid match — see
	// ftsNarrowing — so the common case keeps its index scan.
	for _, body := range c.Body {
		clauses = append(clauses, b.substringClause(body, bodyScopeBody))
	}
	for _, txt := range c.Text {
		clauses = append(clauses, b.substringClause(txt, bodyScopeText))
	}

	// CONDSTORE MODSEQ: SearchCriteria.ModSeq filters to rows with
	// mod_seq >= the value (RFC 7162 §3.1.5). Metadata-name /
	// metadata-type variants are not implemented; we honor the plain
	// MODSEQ form which is by far the most common in QRESYNC clients.
	if c.ModSeq != nil && c.ModSeq.ModSeq > 0 {
		clauses = append(clauses,
			fmt.Sprintf("mod_seq >= %s", b.placeholder(int64(c.ModSeq.ModSeq))))
	}

	// NOT: every nested criteria gets negated and AND'd in.
	for i := range c.Not {
		nested, err := b.buildCriteria(&c.Not[i])
		if err != nil {
			return "", err
		}
		if nested == "" {
			// An empty nested criterion means ALL, which the caller
			// translates to TRUE — so its negation is FALSE (RA6X-011).
			//
			// Skipping it here made `NOT ALL` a no-op, and an expression with
			// no other terms then became TRUE: `SEARCH NOT ALL` returned EVERY
			// message instead of none. The OR arm below already substitutes
			// TRUE for an empty operand, which is the same convention read the
			// other way round — the two simply disagreed.
			clauses = append(clauses, "FALSE")
			continue
		}
		clauses = append(clauses, "NOT ("+nested+")")
	}

	// OR: pairs of subtrees, joined into an "(A OR B)" clause.
	for _, pair := range c.Or {
		left, err := b.buildCriteria(&pair[0])
		if err != nil {
			return "", err
		}
		right, err := b.buildCriteria(&pair[1])
		if err != nil {
			return "", err
		}
		if left == "" {
			left = "TRUE"
		}
		if right == "" {
			right = "TRUE"
		}
		clauses = append(clauses, fmt.Sprintf("((%s) OR (%s))", left, right))
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " AND "), nil
}

// escapeLike escapes the SQL LIKE metacharacters (backslash, percent,
// underscore) in a user-supplied value so they match literally. The
// emitted clause carries an explicit ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// maxSeqExpr / maxUIDExpr are correlated subqueries that resolve the
// folder's highest sequence number / UID at query time. $1 is always the
// selected folder id (searchSQL seeds args with it).
const (
	maxSeqExpr = `(SELECT count(*) FROM messages WHERE folder_id = $1)`
	maxUIDExpr = `(SELECT COALESCE(max(uid), 0) FROM messages WHERE folder_id = $1)`
)

// seqSetClause emits `seqnum IN (...)` / range disjunctions matching
// the IMAP SeqSet shape. It references the synthesized seqnum column, so it
// forces the row_number() window on (R-041).
func (b *searchSQL) seqSetClause(s imap.SeqSet) string {
	if b.view != nil {
		var uids []int64
		for i, uid := range b.view.uids {
			if seqSetContains(s, uint32(i+1), uint32(b.view.len())) {
				uids = append(uids, uid)
			}
		}
		return "uid = ANY(" + b.placeholder(uids) + "::bigint[])"
	}
	b.usesSeqNum = true
	return rangeClause(rangesOfSeqSet(s), "seqnum", maxSeqExpr,
		func(v any) string { return b.placeholder(v) })
}

// uidSetClause emits the same shape against the `uid` column.
func (b *searchSQL) uidSetClause(s imap.UIDSet) string {
	maxExpr := maxUIDExpr
	if b.view != nil {
		maxExpr = fmt.Sprintf("%d::bigint", b.view.maxUID())
	}
	return rangeClause(rangesOfUIDSet(s), "uid", maxExpr,
		func(v any) string { return b.placeholder(v) })
}

type numRange struct {
	start, stop int64 // 0 stop means "*"
	startStar   bool  // start is "*"
}

func rangesOfSeqSet(s imap.SeqSet) []numRange {
	out := make([]numRange, 0, len(s))
	for _, r := range s {
		out = append(out, numRange{start: int64(r.Start), stop: int64(r.Stop), startStar: r.Start == 0})
	}
	return out
}

func rangesOfUIDSet(s imap.UIDSet) []numRange {
	out := make([]numRange, 0, len(s))
	for _, r := range s {
		out = append(out, numRange{start: int64(r.Start), stop: int64(r.Stop), startStar: r.Start == 0})
	}
	return out
}

// rangeClause builds a parenthesized OR over the set's ranges. RFC 3501 /
// 9051 message-set semantics for "*": bare "*" is exactly the highest
// message; "N:*" and "*:N" are equivalent and span min(N, max)..max — a
// non-empty folder always matches its last message for such ranges.
// maxExpr is a correlated subquery resolving the folder's max for col.
func rangeClause(ranges []numRange, col, maxExpr string, ph func(v any) string) string {
	if len(ranges) == 0 {
		return "TRUE"
	}
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		switch {
		case r.startStar && r.stop == 0:
			// Bare "*": the single highest message, not the whole folder.
			parts = append(parts, fmt.Sprintf("%s = %s", col, maxExpr))
		case r.stop == 0:
			// "N:*" spans min(N, max)..max.
			parts = append(parts, fmt.Sprintf("%s >= LEAST(%s, %s)", col, ph(r.start), maxExpr))
		case r.startStar:
			// "*:N" is the same range as "N:*" per RFC.
			parts = append(parts, fmt.Sprintf("%s >= LEAST(%s, %s)", col, ph(r.stop), maxExpr))
		case r.start == r.stop:
			parts = append(parts, fmt.Sprintf("%s = %s", col, ph(r.start)))
		default:
			s := r.start
			e := r.stop
			if s > e {
				s, e = e, s
			}
			parts = append(parts, fmt.Sprintf("%s BETWEEN %s AND %s", col, ph(s), ph(e)))
		}
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// bodyScope distinguishes IMAP's two text-search scopes (RA6X-026).
type bodyScope int

const (
	// bodyScopeBody is SEARCH BODY: the message BODY only.
	bodyScopeBody bodyScope = iota
	// bodyScopeText is SEARCH TEXT: the entire message, headers included.
	bodyScopeText
)

// substringClause builds the predicate for one BODY or TEXT term.
//
// IMAP string search is substring matching, case-insensitive, over the
// specified scope, and the clause below says exactly that.
//
// NO FTS PRE-FILTER IS APPLIED, deliberately. This server lets an index narrow
// the candidates only where doing so cannot exclude valid matches, and with
// the index as it stands there is no term for which that holds:
//
//   - the `fts` vector covers subject and text_body, while BODY must also
//     search html_body — a word present only in the HTML part is a valid match
//     the vector does not know about;
//   - TEXT must also search headers, which the vector excludes entirely;
//   - since RA6X-030 the vector is built from a bounded prefix, so a match
//     beyond it is valid and unindexed.
//
// A pg_trgm index over the same columns would make substring search indexable
// and is the right answer if these scans ever become a problem; adding an
// extension is a schema decision, not one SEARCH makes on its own. Until then
// a sequential scan that is CORRECT beats an index scan that silently drops
// matches, which is what the previous implementation did.
//
// An EMPTY search string matches every message: it is a substring of anything,
// which is what RFC 9051 §6.4.4's "messages that contain the specified string"
// means for the empty string. FTS silently matched nothing.
func (b *searchSQL) substringClause(term string, scope bodyScope) string {
	if term == "" {
		return "TRUE"
	}
	pattern := b.placeholder("%" + escapeLike(strings.ToLower(term)) + "%")

	var fields []string
	switch scope {
	case bodyScopeBody:
		// The body projections only. Subject is deliberately absent: including
		// it is what let BODY match a subject-only word.
		fields = []string{
			"LOWER(COALESCE(text_body, ''))",
			"LOWER(COALESCE(html_body, ''))",
		}
	case bodyScopeText:
		// The whole message: every header value as well as the body. The
		// projection contains decoded field names and values in canonical order,
		// separated as RFC header lines. JSON punctuation and escaping never
		// participate in matching. Body decoding stays identical to ingest.
		fields = []string{
			"LOWER(COALESCE(text_body, ''))",
			"LOWER(COALESCE(html_body, ''))",
			"LOWER(COALESCE(subject, ''))",
			"LOWER(" + headerTextProjection + ")",
		}
	}

	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf(`%s LIKE %s ESCAPE '\'`, f, pattern))
	}
	clause := "(" + strings.Join(parts, " OR ") + ")"

	return clause
}

// Canonical searchable header projection: sorted names, repeated values in
// original value order, one Name: value line per field. The JSONB storage syntax
// is not message text. Existing rows require no schema/data rewrite.
const headerTextProjection = `(SELECT COALESCE(string_agg(h.key || ': ' || v.value, E'\n' ORDER BY h.key, v.ord), '')
 FROM jsonb_each(CASE WHEN jsonb_typeof(headers)='object' THEN headers ELSE '{}'::jsonb END) h
 CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(h.value)='array' THEN h.value ELSE jsonb_build_array(h.value) END) WITH ORDINALITY v(value,ord))`
