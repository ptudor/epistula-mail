package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/epistula-mail/database/maildir"
)

func TestCheckpointResumeKey(t *testing.T) {
	tests := []struct {
		name string
		ck   checkpoint
		want string
	}{
		{"empty", checkpoint{}, ""},
		{"key form", checkpoint{LastKey: "cur/123.host:2,S"}, "cur/123.host:2,S"},
		{"key wins over legacy", checkpoint{LastKey: "new/9", LastPath: "/x/cur/1"}, "new/9"},
		{"legacy absolute", checkpoint{LastPath: "/var/mail/Maildir/cur/123.host:2,S"}, "cur/123.host:2,S"},
		{"legacy trailing slash dir", checkpoint{LastPath: "/var/mail/Maildir/new/456.host"}, "new/456.host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ck.resumeKey(); got != tt.want {
				t.Errorf("resumeKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWalkEmitsAscendingKeys pins the invariant the import checkpoint
// depends on: Walk yields entries in strictly ascending Entry.Key order,
// across both cur/ and new/.
func TestWalkEmitsAscendingKeys(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]string{
		"cur": {"100.a:2,S", "300.c:2,", "200.b:2,F"},
		"new": {"150.x", "050.y"},
		"tmp": {"999.ignored"},
	}
	for sub, names := range files {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(root, sub, n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	var keys []string
	if err := maildir.Walk(root, func(e maildir.Entry) error {
		keys = append(keys, e.Key())
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(keys) != 5 {
		t.Fatalf("walked %d entries, want 5 (tmp/ must be skipped): %v", len(keys), keys)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("keys not strictly ascending at %d: %q >= %q (full: %v)", i, keys[i-1], keys[i], keys)
		}
	}
}
