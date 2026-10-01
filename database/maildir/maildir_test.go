package maildir

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	cases := []struct {
		filename string
		want     Flags
	}{
		{"1234567.M.host", Flags{}},
		{"1234.M.host:2,", Flags{}},
		{"1234.M.host:2,S", Flags{Seen: true}},
		{"1234.M.host:2,FR", Flags{Flagged: true, Answered: true}},
		{"1234.M.host:2,DFPRST", Flags{Draft: true, Flagged: true, Passed: true, Answered: true, Seen: true, Trashed: true}},
		{"1234.M.host:2,xyz", Flags{}}, // unknown letters ignored
	}
	for _, c := range cases {
		got := ParseFlags(c.filename)
		if got != c.want {
			t.Errorf("ParseFlags(%q) = %+v, want %+v", c.filename, got, c.want)
		}
	}
}

func TestIMAPFlags(t *testing.T) {
	f := Flags{Seen: true, Flagged: true, Trashed: true, Passed: true}
	got := f.IMAP()
	want := []string{`\Flagged`, `\Seen`, `\Deleted`, "$Forwarded"}
	if !slices.Equal(got, want) {
		t.Errorf("IMAP() = %v, want %v", got, want)
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "cur")
	mustMkdir(t, root, "new")
	mustMkdir(t, root, "tmp")

	// cur/ entries, intentionally out of alphabetical order on disk.
	mustWrite(t, root, "cur/zlast.host:2,S", "z body")
	mustWrite(t, root, "cur/afirst.host:2,FR", "a body")
	// new/ entries
	mustWrite(t, root, "new/nfile.host", "n body")
	// tmp/ should be skipped
	mustWrite(t, root, "tmp/should-not-see", "t body")
	// dot-prefix should be skipped
	mustWrite(t, root, "cur/.hidden", "h body")

	var paths []string
	var flagsSeen []Flags
	err := Walk(root, func(e Entry) error {
		paths = append(paths, e.BaseName)
		flagsSeen = append(flagsSeen, e.Flags)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// cur/ alphabetical, then new/ alphabetical, tmp/ + dotfiles skipped.
	want := []string{
		"afirst.host:2,FR",
		"zlast.host:2,S",
		"nfile.host",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	if !flagsSeen[0].Flagged || !flagsSeen[0].Answered {
		t.Errorf("flagsSeen[0] = %+v", flagsSeen[0])
	}
	if !flagsSeen[1].Seen {
		t.Errorf("flagsSeen[1] = %+v", flagsSeen[1])
	}
	if flagsSeen[2] != (Flags{}) {
		t.Errorf("flagsSeen[2] = %+v, want zero", flagsSeen[2])
	}
}

func TestWalkErrStop(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "cur")
	mustWrite(t, root, "cur/a", "a")
	mustWrite(t, root, "cur/b", "b")
	mustWrite(t, root, "cur/c", "c")

	count := 0
	err := Walk(root, func(e Entry) error {
		count++
		if count == 2 {
			return ErrStop
		}
		return nil
	})
	if err != nil {
		t.Errorf("Walk with ErrStop returned err: %v", err)
	}
	if count != 2 {
		t.Errorf("visited %d entries, want 2", count)
	}
}

func TestWalkMissingSubdir(t *testing.T) {
	root := t.TempDir()
	// Only create cur/, no new/. Walk should not error.
	mustMkdir(t, root, "cur")
	mustWrite(t, root, "cur/x", "x")
	count := 0
	err := Walk(root, func(e Entry) error { count++; return nil })
	if err != nil {
		t.Errorf("Walk: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestWalkRootNotDirectory(t *testing.T) {
	if err := Walk("", func(Entry) error { return nil }); err == nil {
		t.Error("Walk(\"\"): expected error")
	}
	if err := Walk("/nonexistent/path/here", func(Entry) error { return nil }); err == nil {
		t.Error("Walk(missing): expected error")
	}
}

func TestEntryModTime(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "cur")
	path := filepath.Join(root, "cur", "f")
	mustWrite(t, root, "cur/f", "x")
	mt := time.Date(2003, 1, 15, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	var got time.Time
	err := Walk(root, func(e Entry) error {
		got = e.ModTime
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !got.Equal(mt) {
		t.Errorf("ModTime = %v, want %v", got, mt)
	}
}

// Test helpers.

func mustMkdir(t *testing.T, root, sub string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", sub, err)
	}
}

func mustWrite(t *testing.T, root, relpath, contents string) {
	t.Helper()
	full := filepath.Join(root, relpath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", relpath, err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", relpath, err)
	}
}
