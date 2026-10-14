package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/epistula-mail/database/ingest"
)

func TestConfiguredIngestLimits(t *testing.T) {
	if got := DefaultConfig().Limits.IngestLimits(); got != ingest.DefaultLimits() {
		t.Fatalf("defaults: %+v", got)
	}
	p := filepath.Join(t.TempDir(), "imap.toml")
	if err := os.WriteFile(p, []byte(`[postgres]
dsn = "postgres://localhost/mail?sslmode=disable"
[limits]
max_message_bytes = 1000000
max_mime_depth = 20
max_mime_parts = 400
max_header_bytes = 32000
max_header_section_bytes = 500000
max_transfer_expansion = 30
`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := ingest.Limits{MaxMessageBytes: 1000000, MaxMimeDepth: 20, MaxMimeParts: 400, MaxHeaderBytes: 32000, MaxHeaderSectionBytes: 500000, MaxTransferExpansion: 30}
	if got := c.Limits.IngestLimits(); got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
