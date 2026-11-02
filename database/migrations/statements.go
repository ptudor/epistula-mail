package migrations

import (
	"fmt"
	"regexp"
	"strings"
)

// A migration that must run outside a transaction declares so on its first
// line:
//
//	-- migrate: no-transaction
//
// Such a migration is split into statements and each is executed on its own,
// so CREATE INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY — which PostgreSQL
// refuses inside a transaction block — can build or remove an index on a hot
// table without taking the lock that stops delivery and IMAP for the duration
// of the build. The version row is recorded only after every statement has
// succeeded. A statement that fails leaves the earlier ones committed, so every
// statement of such a migration must be idempotent (IF NOT EXISTS, IF EXISTS,
// a WHERE clause that excludes already-converted rows) and the run can simply
// be repeated. A group of statements that must commit together may be wrapped
// in explicit BEGIN; ... COMMIT; lines: they run on the one connection the
// migration holds.
const directiveNoTransaction = "no-transaction"

var directiveRe = regexp.MustCompile(`^--\s*migrate:\s*(\S+)\s*$`)

// parseDirectives reads the `-- migrate:` directives from a migration's
// leading comment lines. Scanning stops at the first line that is neither a
// comment nor blank, so a directive buried below SQL is a mistake the test
// suite would catch rather than a silently ignored one.
func parseDirectives(sql string) (noTransaction bool, err error) {
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		m := directiveRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case directiveNoTransaction:
			noTransaction = true
		default:
			return false, &DirectiveError{Directive: m[1]}
		}
	}
	return noTransaction, nil
}

// DirectiveError reports a `-- migrate:` directive the runner does not know.
type DirectiveError struct{ Directive string }

func (e *DirectiveError) Error() string {
	return fmt.Sprintf("unknown migrate directive %q", e.Directive)
}

// splitStatements splits sql into its top-level statements at semicolons. It
// honours what PostgreSQL's own lexer honours: single-quoted strings (with ”
// doubling), E” strings (with backslash escapes), double-quoted identifiers,
// dollar-quoted strings with or without a tag, `--` line comments and nested
// `/* */` block comments. A chunk that holds only whitespace and comments is
// dropped; comments inside a statement travel with it, where the server
// ignores them and an operator reading the log can still see them.
func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	hasSQL := false
	emit := func() {
		if hasSQL {
			out = append(out, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
		hasSQL = false
	}

	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			// Line comment: copy through end of line.
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				cur.WriteString(sql[i:])
				i = n
			} else {
				cur.WriteString(sql[i : i+j+1])
				i += j + 1
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			depth := 0
			j := i
			for j < n {
				if sql[j] == '/' && j+1 < n && sql[j+1] == '*' {
					depth++
					j += 2
					continue
				}
				if sql[j] == '*' && j+1 < n && sql[j+1] == '/' {
					depth--
					j += 2
					if depth == 0 {
						break
					}
					continue
				}
				j++
			}
			cur.WriteString(sql[i:j])
			i = j
		case c == '\'':
			escapes := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') &&
				(i < 2 || !isIdentByte(sql[i-2]))
			j := i + 1
			for j < n {
				if escapes && sql[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if sql[j] == '\'' {
					j++
					break
				}
				j++
			}
			cur.WriteString(sql[i:j])
			hasSQL = true
			i = j
		case c == '"':
			j := i + 1
			for j < n && sql[j] != '"' {
				j++
			}
			if j < n {
				j++
			}
			cur.WriteString(sql[i:j])
			hasSQL = true
			i = j
		case c == '$':
			if tag, ok := dollarTag(sql[i:]); ok {
				end := strings.Index(sql[i+len(tag):], tag)
				var j int
				if end < 0 {
					j = n
				} else {
					j = i + len(tag) + end + len(tag)
				}
				cur.WriteString(sql[i:j])
				hasSQL = true
				i = j
			} else {
				cur.WriteByte(c)
				hasSQL = true
				i++
			}
		case c == ';':
			emit()
			i++
		default:
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				hasSQL = true
			}
			cur.WriteByte(c)
			i++
		}
	}
	emit()
	return out
}

// dollarTag reports whether s starts a dollar-quoted string ($$ or $tag$) and
// returns the full delimiter. A positional parameter such as $1 is not one.
func dollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	j := 1
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return "", false
	}
	if j > 1 && s[1] >= '0' && s[1] <= '9' {
		return "", false
	}
	return s[:j+1], true
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

var createIndexConcurrentlyRe = regexp.MustCompile(
	`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[A-Za-z_][A-Za-z0-9_]*)`)

// concurrentIndexNames returns the names of the indexes the statements build
// with CREATE INDEX CONCURRENTLY, as written (a quoted name keeps its quotes).
func concurrentIndexNames(stmts []string) []string {
	var names []string
	for _, st := range stmts {
		if m := createIndexConcurrentlyRe.FindStringSubmatch(stripLeadingComments(st)); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// stripLeadingComments drops the comment and blank lines a statement begins
// with, so a pattern anchored at the start sees the SQL.
func stripLeadingComments(st string) string {
	lines := strings.Split(st, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}
