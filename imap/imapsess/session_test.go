package imapsess

import (
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"
)

var (
	_ imapserver.SessionNamespace = (*Session)(nil)
	_ imapserver.SessionMove      = (*Session)(nil)
	_ imapserver.SessionIMAP4rev2 = (*Session)(nil)
)

func TestPatternMatchesUsesIMAPListGlobs(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		ref      string
		want     bool
	}{
		{name: "INBOX", patterns: []string{"INBOX"}, want: true},
		{name: "Archive/2025/Q1", patterns: []string{"Archive/*"}, want: true},
		{name: "Archive/2025/Q1", patterns: []string{"Archive/%"}, want: false},
		{name: "Archive/2025", patterns: []string{"Archive/%"}, want: true},
		{name: "Archive/2025/Q1", patterns: []string{"%"}, want: false},
		{name: "Sent", patterns: []string{"%"}, want: true},
		{name: "Archive/2025", patterns: []string{"%"}, ref: "Archive", want: true},
		{name: "Projects/Alpha", patterns: []string{"Archive/%", "Projects/%"}, want: true},
	}

	for _, tt := range tests {
		if got := patternMatches(tt.name, tt.patterns, tt.ref); got != tt.want {
			t.Fatalf("patternMatches(%q, %#v, %q) = %v, want %v", tt.name, tt.patterns, tt.ref, got, tt.want)
		}
	}
}
