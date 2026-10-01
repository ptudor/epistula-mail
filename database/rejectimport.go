package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// runAdminRejectExport reads CSV reject rules and emits Postfix-compatible
// policy files. Spam filtering belongs upstream of the LDA, so this is an
// optional policy-conversion command rather than a delivery-time filter.
//
// Output format depends on the rule's `scope` column:
//
//   - scope='recipient' rows become `check_recipient_access` entries in
//     the form `regexp:<domain>` patterns. Postfix evaluates them per
//     recipient at smtpd time and can REJECT before the LDA is invoked.
//
//   - scope='sender' / scope='both' rows become `header_checks` regex
//     patterns matching the From: header. Postfix evaluates header_checks
//     in cleanup(8) before queueing.
//
// Operator workflow:
//  1. Prepare CSV rules with the documented columns.
//  2. epistula-database admin reject-export -in rejects.csv -out-dir /tmp/postfix-rules
//  3. Wire the two output files into main.cf:
//     header_checks       = regexp:/etc/postfix/header_checks.cf
//     smtpd_recipient_restrictions = ..., check_recipient_access regexp:/etc/postfix/recipient_access.cf, ...
//  4. postmap (not needed for regexp:) and `postfix reload`.
func runAdminRejectExport(args []string) int {
	fs := flag.NewFlagSet("admin reject-export", flag.ContinueOnError)
	in := fs.String("in", "", "CSV input (domain,scope,reject_string[,description]); '-' = stdin")
	outDir := fs.String("out-dir", "", "Output directory (will be created); writes header_checks.cf + recipient_access.cf")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *in == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "reject-export: -in and -out-dir are required")
		fmt.Fprint(os.Stderr, helpRejectExport)
		return EX_USAGE
	}

	reader, closer, err := openCSVInput(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open input: %v\n", err)
		return EX_NOINPUT
	}
	defer closer.Close()

	rules, err := parseRejectCSV(reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		return EX_DATAERR
	}
	if len(rules) == 0 {
		fmt.Fprintln(os.Stderr, "no rules found in input")
		return EX_OK
	}

	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		return EX_CANTCREAT
	}

	headerPath := *outDir + "/header_checks.cf"
	recipPath := *outDir + "/recipient_access.cf"

	hc, rc, summary := renderRejectRules(rules)
	if err := writeFileExclusive(headerPath, hc); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", headerPath, err)
		return EX_IOERR
	}
	if err := writeFileExclusive(recipPath, rc); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", recipPath, err)
		return EX_IOERR
	}

	fmt.Printf("wrote %s (%d sender/both rules)\n", headerPath, summary.headerCount)
	fmt.Printf("wrote %s (%d recipient rules)\n", recipPath, summary.recipientCount)
	if summary.skipped > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d malformed rows (see stderr above)\n", summary.skipped)
	}
	fmt.Println("\nTo activate, append to /usr/local/etc/postfix/main.cf:")
	fmt.Printf("    header_checks = regexp:%s\n", headerPath)
	fmt.Printf("    smtpd_recipient_restrictions = ..., check_recipient_access regexp:%s, ...\n", recipPath)
	fmt.Println("then `postfix reload`. No postmap needed (regexp tables are read directly).")
	return EX_OK
}

const helpRejectExport = `
The expected CSV header is one of:
    domain,scope,reject_string
    domain,scope,reject_string,description

Rules kept by a previous system can be exported to this CSV with a query
such as the following (adjust the table and column names to its schema):

    -- Postgres
    \copy (
      SELECT d.name AS domain, r.scope, r.reject_string, COALESCE(r.description, '')
        FROM reject_rules r JOIN domains d ON d.id = r.domain_id
    ) TO 'rejects.csv' CSV HEADER

    -- MySQL/MariaDB
    SELECT 'domain,scope,reject_string,description'
    UNION ALL
    SELECT CONCAT(d.name,',',r.scope,',',
                  CONCAT('"', REPLACE(r.reject_string,'"','""'),'"'),',',
                  CONCAT('"', REPLACE(COALESCE(r.description,''),'"','""'),'"'))
      FROM reject_rules r JOIN domains d ON d.id = r.domain_id
      INTO OUTFILE '/tmp/rejects.csv';

`

type rejectRule struct {
	Domain      string
	Scope       string // 'sender' | 'recipient' | 'both'
	Substring   string
	Description string
}

type rejectSummary struct {
	headerCount    int
	recipientCount int
	skipped        int
}

func openCSVInput(path string) (*csv.Reader, io.Closer, error) {
	if path == "-" {
		r := csv.NewReader(os.Stdin)
		r.FieldsPerRecord = -1 // match the file path: tolerate variable width
		return r, io.NopCloser(os.Stdin), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate the optional description column
	return r, f, nil
}

// parseRejectCSV reads the header row, then every subsequent row. Rows
// with empty substrings or unrecognized scopes are skipped with a log to
// stderr — we do not fail the whole export for one bad row, since the
// operator will want the maximum salvageable output.
func parseRejectCSV(r *csv.Reader) ([]rejectRule, error) {
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, required := range []string{"domain", "scope", "reject_string"} {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("missing required column %q", required)
		}
	}
	descIdx, hasDesc := col["description"]

	// FieldsPerRecord = -1 lets a row be shorter than the header, so guard the
	// highest required column index before positional access — otherwise a
	// truncated row ("foo,sender") panics with index-out-of-range instead of
	// being skipped (R-022).
	maxRequiredIdx := col["domain"]
	if col["scope"] > maxRequiredIdx {
		maxRequiredIdx = col["scope"]
	}
	if col["reject_string"] > maxRequiredIdx {
		maxRequiredIdx = col["reject_string"]
	}

	var rules []rejectRule
	line := 1
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(row) <= maxRequiredIdx {
			fmt.Fprintf(os.Stderr, "line %d: too few fields, skipping\n", line)
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(row[col["domain"]]))
		scope := strings.ToLower(strings.TrimSpace(row[col["scope"]]))
		substr := strings.TrimSpace(row[col["reject_string"]])
		desc := ""
		if hasDesc && descIdx < len(row) {
			desc = strings.TrimSpace(row[descIdx])
		}
		if domain == "" || substr == "" {
			fmt.Fprintf(os.Stderr, "line %d: empty domain or reject_string, skipping\n", line)
			continue
		}
		// Control characters in a domain or pattern are REJECTED, with a
		// precise row/field error, before either output file is created
		// (RA6X-061).
		//
		// A quoted CSV field may legitimately contain an embedded newline, and
		// TrimSpace only removes outer whitespace: regexp.QuoteMeta and the
		// slash escaping leave an interior newline intact, so a pattern
		// containing one SPLIT the generated regexp across two Postfix policy
		// lines. The first half becomes a rule that matches something else and
		// the second becomes a syntax error Postfix logs and ignores — a rule
		// the operator meant to enforce silently stops being enforced.
		//
		// Sanitising the pattern instead would be worse: silently removing a
		// character changes what the rule matches, which is the same failure
		// with no error message.
		if bad, field := firstControlField(domain, substr); bad {
			fmt.Fprintf(os.Stderr,
				"line %d: %s contains a control character (embedded newline, CR, NUL or tab); "+
					"refusing to generate policy from it\n", line, field)
			return nil, fmt.Errorf("line %d: %s contains a control character", line, field)
		}
		switch scope {
		case "sender", "recipient", "both":
		default:
			fmt.Fprintf(os.Stderr, "line %d: unknown scope %q, skipping\n", line, scope)
			continue
		}
		rules = append(rules, rejectRule{
			Domain:      domain,
			Scope:       scope,
			Substring:   substr,
			Description: desc,
		})
	}
	return rules, nil
}

// rejectReplyText is the fixed SMTP reply Postfix returns to the rejected
// party. Postfix treats everything after REJECT as reply text sent to the
// sender/client, so the operator's candid rule description (names, "known
// abuser", internal reasons) must NEVER go here — it would leak verbatim on
// every rejection (R-020). Provenance goes on a preceding `#` comment line.
const rejectReplyText = "Message rejected by local policy"

// postfixEscape quotes a substring for use inside a /.../ delimited Postfix
// regexp pattern. regexp.QuoteMeta handles regex metacharacters; we then also
// backslash-escape '/', which is NOT a regex metachar (so QuoteMeta leaves it)
// but would prematurely terminate the /pattern/ delimiter — Postfix would log
// a warning and silently drop the rule, so spam the rule was meant to block
// would start getting through (R-021).
func postfixEscape(s string) string {
	return strings.ReplaceAll(regexp.QuoteMeta(s), "/", `\/`)
}

// renderRejectRules emits two Postfix policy bodies and a summary.
//
// The header_checks output uses /^From:.*<SUBSTRING>/ REJECT lines for
// sender + both rows. The recipient_access output uses /^...@<domain>$/
// REJECT lines for recipient + both rows. Each pattern is preceded by a `#`
// comment line carrying the source rule's provenance/description (the REJECT
// reply itself is a fixed neutral string). Pattern lines are deduplicated and
// sorted so re-runs produce stable diffs; the first-seen comment wins.
func renderRejectRules(rules []rejectRule) (headerChecks, recipientAccess string, summary rejectSummary) {
	var (
		header   []string
		recip    []string
		seenH    = map[string]bool{}
		seenR    = map[string]bool{}
		commentH = map[string]string{} // pattern line → provenance comment (first seen)
		commentR = map[string]string{}
		nowLabel = time.Now().UTC().Format("2006-01-02")
	)

	for _, r := range rules {
		switch r.Scope {
		case "sender", "both":
			line := fmt.Sprintf("/^From:.*%s/ REJECT %s", postfixEscape(r.Substring), rejectReplyText)
			if !seenH[line] {
				seenH[line] = true
				commentH[line] = commentFor(r)
				header = append(header, line)
				summary.headerCount++
			}
		}
		switch r.Scope {
		case "recipient", "both":
			// Postfix check_recipient_access keys are address-shaped; regexp:
			// tables match against the recipient address. A rule substring
			// containing '@' is a full address — the old .*<substr>.*@<domain>$
			// wrapper produced a two-'@' pattern (^.*user@dom.*@domain$) that
			// could never match, silently dropping the rule (R-021). Anchor
			// such a substring as the whole recipient address instead. A
			// plain substring keeps the match-anywhere-scoped-to-domain form.
			var line string
			if strings.Contains(r.Substring, "@") {
				line = fmt.Sprintf("/^%s$/ REJECT %s", postfixEscape(r.Substring), rejectReplyText)
			} else {
				line = fmt.Sprintf("/^.*%s.*@%s$/ REJECT %s",
					postfixEscape(r.Substring), postfixEscape(r.Domain), rejectReplyText)
			}
			if !seenR[line] {
				seenR[line] = true
				commentR[line] = commentFor(r)
				recip = append(recip, line)
				summary.recipientCount++
			}
		}
	}

	sort.Strings(header)
	sort.Strings(recip)

	var hb, rb strings.Builder
	preamble := func(b *strings.Builder, kind string) {
		fmt.Fprintf(b, "# Generated by epistula-database admin reject-export on %s\n", nowLabel)
		fmt.Fprintf(b, "# Source: CSV reject rules (domain,scope,reject_string[,description]).\n")
		fmt.Fprintf(b, "# Activate with:  %s = regexp:%s\n", kind, "<this-file>")
		fmt.Fprintf(b, "# Lines are regexp patterns; PCRE flavor is not required.\n\n")
	}
	preamble(&hb, "header_checks")
	for _, l := range header {
		fmt.Fprintf(&hb, "# %s\n", commentH[l])
		hb.WriteString(l)
		hb.WriteByte('\n')
	}
	preamble(&rb, "check_recipient_access")
	for _, l := range recip {
		fmt.Fprintf(&rb, "# %s\n", commentR[l])
		rb.WriteString(l)
		rb.WriteByte('\n')
	}
	return hb.String(), rb.String(), summary
}

// commentFor produces a short trailing comment so the operator can trace
// each line back to its CSV rule. Description text is sanitized to a
// single line of safe characters — newlines in a Postfix policy line
// would break the parser.
func commentFor(r rejectRule) string {
	// EVERY data component is sanitized, not just the description (RA6X-061).
	// Domain was interpolated raw into the same comment line, so a
	// control-bearing domain injected into the generated policy through the
	// one field nobody was checking. The scope is from a fixed set and needs
	// no sanitizing, but goes through the same call so the rule is "sanitize
	// everything that came from the CSV" rather than a list to keep in sync.
	c := fmt.Sprintf("%s rule (%s)", safeComment(r.Scope), safeComment(r.Domain))
	if r.Description != "" {
		c += ": " + safeComment(r.Description)
	}
	return c
}

// firstControlField reports the first of (domain, pattern) that contains a
// character which must never reach a generated Postfix policy line.
//
// Anything below U+0020, plus DEL: a newline or CR splits the line, NUL
// truncates it in C, and a tab is a field separator in some Postfix table
// formats. The check is on the value AFTER trimming, so ordinary leading and
// trailing whitespace in a spreadsheet export is still tolerated.
func firstControlField(domain, pattern string) (bool, string) {
	if i := strings.IndexFunc(domain, isPolicyControl); i >= 0 {
		return true, "domain"
	}
	if i := strings.IndexFunc(pattern, isPolicyControl); i >= 0 {
		return true, "reject_string"
	}
	return false, ""
}

func isPolicyControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// safeComment strips control characters and CRLF so a malicious or
// malformed description in the source CSV cannot inject extra Postfix
// directives into the output.
func safeComment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 120 {
		// Truncate on a rune boundary so a multibyte character at the cut
		// point can't leave invalid UTF-8 in the generated Postfix file.
		runes := []rune(out)
		if len(runes) > 117 {
			runes = runes[:117]
		}
		out = string(runes) + "..."
	}
	return out
}

// writeFileExclusive writes content to path with O_EXCL so an existing
// file is not silently overwritten — the operator must move the previous
// run out of the way first. Permissions: 0640 so Postfix's group can read.
func writeFileExclusive(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	return f.Sync()
}
