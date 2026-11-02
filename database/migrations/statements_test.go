package migrations

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplitStatementsHonoursQuotingAndComments(t *testing.T) {
	sql := `-- migrate: no-transaction
-- A header comment; the semicolon in it (like this one;) must not split.
CREATE INDEX CONCURRENTLY IF NOT EXISTS a ON t (x); -- trailing; comment
/* a block; comment
   /* nested; */ still a comment; */
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'k;x') THEN
        ALTER TABLE t ADD CONSTRAINT k UNIQUE USING INDEX a;
    END IF;
END
$$;
SELECT 'it''s; fine', E'esc\'aped; too', "a;quoted" FROM t;
SELECT $tag$ dollar; quoted $tag$, $1;
`
	got := splitStatements(sql)
	want := []string{
		"-- migrate: no-transaction\n-- A header comment; the semicolon in it (like this one;) must not split.\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a ON t (x)",
		"-- trailing; comment\n/* a block; comment\n   /* nested; */ still a comment; */\nDO $$\nBEGIN\n    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'k;x') THEN\n        ALTER TABLE t ADD CONSTRAINT k UNIQUE USING INDEX a;\n    END IF;\nEND\n$$",
		"SELECT 'it''s; fine', E'esc\\'aped; too', \"a;quoted\" FROM t",
		"SELECT $tag$ dollar; quoted $tag$, $1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitStatements:\n got %q\nwant %q", got, want)
	}
}

func TestSplitStatementsDropsCommentOnlyChunks(t *testing.T) {
	got := splitStatements("-- only a comment\n\n/* and; another */\n")
	if len(got) != 0 {
		t.Fatalf("comment-only input produced statements: %q", got)
	}
	got = splitStatements("SELECT 1;\n-- trailing comment after the last statement\n")
	if len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("trailing comment became a statement: %q", got)
	}
}

func TestSplitStatementsUnterminatedQuoteRunsToEnd(t *testing.T) {
	// Malformed SQL is the server's to reject; the splitter must not hang or
	// panic on it, and must hand the rest over as one statement.
	got := splitStatements("SELECT 'unterminated; SELECT 2;")
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	got = splitStatements("SELECT $$unterminated; SELECT 2;")
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestParseDirectives(t *testing.T) {
	noTx, err := parseDirectives("-- migrate: no-transaction\n-- more\nSELECT 1;")
	if err != nil || !noTx {
		t.Fatalf("no-transaction directive: noTx=%v err=%v", noTx, err)
	}
	noTx, err = parseDirectives("-- plain comment\nSELECT 1;")
	if err != nil || noTx {
		t.Fatalf("no directive: noTx=%v err=%v", noTx, err)
	}
	// A directive below the first SQL line is not a directive: the header is
	// the contract.
	noTx, err = parseDirectives("SELECT 1;\n-- migrate: no-transaction\n")
	if err != nil || noTx {
		t.Fatalf("late directive honoured: noTx=%v err=%v", noTx, err)
	}
	var derr *DirectiveError
	if _, err := parseDirectives("-- migrate: concurrently\nSELECT 1;"); !errors.As(err, &derr) || derr.Directive != "concurrently" {
		t.Fatalf("unknown directive: err=%v", err)
	}
}

func TestConcurrentIndexNames(t *testing.T) {
	stmts := []string{
		"-- comment\nCREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON t (x)",
		"create unique index concurrently \"Quoted\" on t (y)",
		"CREATE INDEX idx_plain ON t (z)",
		"DROP INDEX CONCURRENTLY IF EXISTS idx_old",
	}
	got := concurrentIndexNames(stmts)
	want := []string{"idx_a", `"Quoted"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEmbeddedMigrationsDeclareTheirMode(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var noTx []int
	for _, m := range all {
		if m.NoTransaction {
			noTx = append(noTx, m.Version)
			if len(splitStatements(m.SQL)) == 0 {
				t.Errorf("migration %03d declares no-transaction but has no statements", m.Version)
			}
		}
	}
	if want := []int{27, 28, 29}; !reflect.DeepEqual(noTx, want) {
		t.Fatalf("no-transaction migrations = %v, want %v", noTx, want)
	}
}
