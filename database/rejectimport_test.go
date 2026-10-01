package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRejectCSVHappyPath(t *testing.T) {
	body := `domain,scope,reject_string,description
ptudor.invalid,sender,@known-spam.tld,banned domain
ptudor.invalid,recipient,old-marketing,
example.invalid,both,FREE PRIZE,classic spam
`
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rules, err := parseRejectCSV(r)
	if err != nil {
		t.Fatalf("parseRejectCSV: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("want 3 rules, got %d", len(rules))
	}
	if rules[0].Scope != "sender" || rules[0].Substring != "@known-spam.tld" {
		t.Errorf("rule[0] wrong: %+v", rules[0])
	}
	if rules[2].Scope != "both" || rules[2].Domain != "example.invalid" {
		t.Errorf("rule[2] wrong: %+v", rules[2])
	}
}

func TestParseRejectCSVRejectsMissingColumn(t *testing.T) {
	body := "domain,reject_string\nptudor.invalid,foo\n"
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	if _, err := parseRejectCSV(r); err == nil {
		t.Fatal("expected error for missing 'scope' column")
	}
}

func TestParseRejectCSVSkipsBadRowsButContinues(t *testing.T) {
	body := `domain,scope,reject_string
ptudor.invalid,sender,good
,,
ptudor.invalid,bogus_scope,whatever
ptudor.invalid,recipient,also-good
`
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rules, err := parseRejectCSV(r)
	if err != nil {
		t.Fatalf("parseRejectCSV: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 surviving rules, got %d: %+v", len(rules), rules)
	}
}

func TestRenderRejectRulesProducesPostfixSyntax(t *testing.T) {
	rules := []rejectRule{
		{Domain: "ptudor.invalid", Scope: "sender", Substring: "@spam.tld", Description: "abuse"},
		{Domain: "ptudor.invalid", Scope: "recipient", Substring: "marketing", Description: ""},
		{Domain: "ptudor.invalid", Scope: "both", Substring: "buy-now", Description: ""},
	}
	hc, rc, sum := renderRejectRules(rules)

	// header_checks: sender + both → 2 lines
	if sum.headerCount != 2 {
		t.Errorf("headerCount=%d want 2", sum.headerCount)
	}
	if !strings.Contains(hc, "/^From:.*@spam\\.tld/ REJECT") {
		t.Errorf("missing quoted From: rule in header_checks:\n%s", hc)
	}
	if !strings.Contains(hc, "/^From:.*buy-now/ REJECT") {
		t.Errorf("missing 'both' rule in header_checks:\n%s", hc)
	}

	// check_recipient_access: recipient + both → 2 lines
	if sum.recipientCount != 2 {
		t.Errorf("recipientCount=%d want 2", sum.recipientCount)
	}
	if !strings.Contains(rc, "marketing") || !strings.Contains(rc, "ptudor\\.invalid") {
		t.Errorf("recipient_access missing expected entries:\n%s", rc)
	}
}

func TestRenderRejectRulesDeduplicates(t *testing.T) {
	rules := []rejectRule{
		{Domain: "ptudor.invalid", Scope: "sender", Substring: "@spam.tld"},
		{Domain: "ptudor.invalid", Scope: "sender", Substring: "@spam.tld"}, // duplicate
	}
	_, _, sum := renderRejectRules(rules)
	if sum.headerCount != 1 {
		t.Errorf("expected duplicate to collapse, got headerCount=%d", sum.headerCount)
	}
}

func TestSafeCommentStripsNewlinesAndControls(t *testing.T) {
	in := "line1\nline2\rinjected\x07bell"
	out := safeComment(in)
	if strings.ContainsAny(out, "\n\r\x07") {
		t.Errorf("safeComment let control chars through: %q", out)
	}
}

// patternLine returns the single /.../ pattern line from a rendered policy
// body (skipping the preamble and the '#' provenance comment lines).
func patternLine(t *testing.T, body string) string {
	t.Helper()
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "/") {
			return ln
		}
	}
	t.Fatalf("no pattern line found in:\n%s", body)
	return ""
}

// unescapedSlashCount counts '/' not preceded by a backslash — the delimiter
// slashes that bound a Postfix /pattern/.
func unescapedSlashCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' && (i == 0 || s[i-1] != '\\') {
			n++
		}
	}
	return n
}

// TestRenderRejectRulesDescriptionOnlyOnCommentLine is the R-020 regression:
// the operator's rule description must appear only on a '#' comment line, never
// as the REJECT reply text Postfix sends to the rejected sender.
func TestRenderRejectRulesDescriptionOnlyOnCommentLine(t *testing.T) {
	rules := []rejectRule{
		{Domain: "ptudor.invalid", Scope: "sender", Substring: "@spam.tld", Description: "known abuser: real name"},
	}
	hc, _, _ := renderRejectRules(rules)
	for _, ln := range strings.Split(hc, "\n") {
		if strings.Contains(ln, "known abuser") && !strings.HasPrefix(strings.TrimSpace(ln), "#") {
			t.Errorf("description leaked onto a non-comment line: %q", ln)
		}
	}
	if !strings.Contains(hc, "# sender rule (ptudor.invalid): known abuser: real name") {
		t.Errorf("provenance comment line missing:\n%s", hc)
	}
	if !strings.Contains(hc, "REJECT "+rejectReplyText) {
		t.Errorf("neutral REJECT reply missing:\n%s", hc)
	}
	if strings.Contains(hc, "REJECT known abuser") || strings.Contains(hc, "REJECT sender rule") {
		t.Errorf("operator note leaked into the REJECT reply:\n%s", hc)
	}
}

// TestRenderRejectRulesEscapesSlash is the R-021 regression: a substring with a
// '/' (URL-fragment rejects are common) must be escaped so it doesn't terminate
// the /pattern/ delimiter and silently disable the rule.
func TestRenderRejectRulesEscapesSlash(t *testing.T) {
	rules := []rejectRule{
		{Domain: "ptudor.invalid", Scope: "sender", Substring: "example.com/track?id="},
	}
	hc, _, _ := renderRejectRules(rules)
	line := patternLine(t, hc)
	if !strings.Contains(line, `example\.com\/track`) {
		t.Errorf("'/' not escaped in pattern: %q", line)
	}
	if n := unescapedSlashCount(line); n != 2 {
		t.Errorf("want exactly 2 delimiter slashes, got %d: %q", n, line)
	}
}

// TestRenderRejectRulesFullAddressRecipient is the R-021 regression for the
// '@' case: a recipient substring containing '@' must anchor as a full address,
// not the never-matching ^.*substr.*@domain$ two-'@' wrapper.
func TestRenderRejectRulesFullAddressRecipient(t *testing.T) {
	rules := []rejectRule{
		{Domain: "ptudor.invalid", Scope: "recipient", Substring: "user@dom.tld"},
	}
	_, rc, _ := renderRejectRules(rules)
	line := patternLine(t, rc)
	if !strings.Contains(line, `/^user@dom\.tld$/`) {
		t.Errorf("expected full-address anchor, got: %q", line)
	}
	if strings.Contains(line, `@ptudor\.invalid$`) {
		t.Errorf("still emitting the never-matching .*@domain$ wrapper: %q", line)
	}
}

// TestParseRejectCSVSkipsShortRow is the R-022 regression: a CSV row with fewer
// fields than the highest required column index must be skipped, not panic.
func TestParseRejectCSVSkipsShortRow(t *testing.T) {
	body := "domain,scope,reject_string\nptudor.invalid,sender,good\nfoo,sender\nptudor.invalid,recipient,ok\n"
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rules, err := parseRejectCSV(r) // must not panic
	if err != nil {
		t.Fatalf("parseRejectCSV: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 surviving rules (short row skipped), got %d: %+v", len(rules), rules)
	}
}

// TestOpenCSVInputStdinToleratesVariableWidth is the R-022 regression for the
// stdin path: it must set FieldsPerRecord = -1 like the file path, so a
// variable-width row doesn't make stdin stricter than a file.
func TestOpenCSVInputStdinToleratesVariableWidth(t *testing.T) {
	body := "domain,scope,reject_string\nptudor.invalid,sender,good,extra-column\n"
	tmp, err := os.CreateTemp(t.TempDir(), "stdin*.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = tmp
	defer func() { os.Stdin = oldStdin }()

	reader, closer, err := openCSVInput("-")
	if err != nil {
		t.Fatalf("openCSVInput: %v", err)
	}
	defer closer.Close()
	rules, err := parseRejectCSV(reader)
	if err != nil {
		t.Fatalf("stdin path rejected a variable-width row (FieldsPerRecord not -1?): %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
}

func TestRejectExportEndToEnd(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "rejects.csv")
	outDir := filepath.Join(dir, "out")
	csvBody := `domain,scope,reject_string,description
ptudor.invalid,sender,@spammer.tld,known abuser
ptudor.invalid,recipient,marketing,
`
	if err := os.WriteFile(inputPath, []byte(csvBody), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}

	code := runAdminRejectExport([]string{"-in", inputPath, "-out-dir", outDir})
	if code != EX_OK {
		t.Fatalf("runAdminRejectExport exited %s (%d)", ExitCodeName(code), code)
	}
	for _, name := range []string{"header_checks.cf", "recipient_access.cf"} {
		st, err := os.Stat(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("output file %s missing: %v", name, err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("%s mode=%v want 0640", name, st.Mode().Perm())
		}
	}

	// Re-running into the same out-dir must refuse to overwrite (we use
	// O_EXCL so the operator's previous run is preserved).
	code = runAdminRejectExport([]string{"-in", inputPath, "-out-dir", outDir})
	if code == EX_OK {
		t.Fatal("re-running into existing out-dir should have failed (O_EXCL)")
	}
}
