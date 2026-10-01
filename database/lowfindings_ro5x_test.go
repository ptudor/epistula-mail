package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestLoggableHeadersRedacts is the RO5X-023 regression: RedactHeader
// implemented the CLAUDE.md contract but was called from nowhere, so the
// contract held only by accident. The LogValuer wrapper makes it hold by
// construction.
func TestLoggableHeadersRedacts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("probe", "headers", LoggableHeaders{
		"X-Auth-Token":        {"abc123"},
		"Authorization":       {"Bearer sekrit"},
		"Cookie":              {"session=xyz"},
		"Proxy-Authorization": {"Basic zzz"},
		"X-Encryption-Key":    {"kkk"},
		"Subject":             {"a normal header"},
		"Message-ID":          {"<m@x.invalid>"},
	})
	out := buf.String()

	for _, secret := range []string{"abc123", "Bearer sekrit", "session=xyz", "Basic zzz", "kkk"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into the log line: %s", secret, out)
		}
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("no redaction marker in the log line: %s", out)
	}
	// Non-secret headers still come through, or the wrapper would be useless.
	if !strings.Contains(out, "a normal header") {
		t.Errorf("non-secret header was dropped: %s", out)
	}
	if !strings.Contains(out, "<m@x.invalid>") {
		t.Errorf("Message-ID was dropped: %s", out)
	}
}

// TestLoggableHeadersDeterministic pins the sorted key order, so two log lines
// for the same headers are byte-identical.
func TestLoggableHeadersDeterministic(t *testing.T) {
	h := LoggableHeaders{"B": {"2"}, "A": {"1"}, "C": {"3"}}
	render := func() string {
		var buf bytes.Buffer
		// ReplaceAttr drops the timestamp, which obviously differs between
		// renders; the point here is the ORDER of the header keys.
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			},
		})).Info("x", "headers", h)
		return buf.String()
	}
	first := render()
	for i := 0; i < 20; i++ {
		if got := render(); got != first {
			t.Fatalf("header rendering is not deterministic:\n%s\n%s", first, got)
		}
	}
}

// TestLoggableHeadersEmpty covers the boundary.
func TestLoggableHeadersEmpty(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "headers", LoggableHeaders{})
	if buf.Len() == 0 {
		t.Error("logging empty headers produced no output at all")
	}
}

// TestValidateDomainName is the RO5X-033 shape validator.
func TestValidateDomainName(t *testing.T) {
	for _, ok := range []string{
		"example.invalid", "a.b.c.example", "x", "xn--80ak6aa92e.com",
		"has-hyphen.example", "123.example", strings.Repeat("a", 63) + ".net",
	} {
		if err := validateDomainName(ok); err != nil {
			t.Errorf("validateDomainName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "example .invalid", "example..invalid", ".example.invalid", "example.invalid.",
		"-lead.example", "trail-.example", "under_score.example",
		"bad\x00null.example", "UPPER.example", // domains are lower-cased first
		strings.Repeat("a", 64) + ".net",  // label too long
		strings.Repeat("a.", 200) + "net", // over 253 bytes
	} {
		if err := validateDomainName(bad); err == nil {
			t.Errorf("validateDomainName(%q) = nil, want an error", bad)
		}
	}
}

// TestNormalizeDomainNFC is the RO5X-033 correctness half: the delivery
// resolver NFC-normalizes the envelope domain, so the admin CLI must too or an
// internationalized domain added in NFD form never matches a delivery.
func TestNormalizeDomainNFC(t *testing.T) {
	// "café" with a combining acute (NFD) vs precomposed é (NFC).
	nfd := "café.example"
	nfc := "café.example"
	if nfd == nfc {
		t.Fatal("fixture is not actually decomposed")
	}
	if got := normalizeDomain(nfd); got != nfc {
		t.Errorf("normalizeDomain(NFD) = %q, want the NFC form %q", got, nfc)
	}
	if got := normalizeDomain(nfc); got != nfc {
		t.Errorf("normalizeDomain(NFC) = %q, want it unchanged", got)
	}
	// Case and whitespace still handled.
	if got := normalizeDomain("  EXAMPLE.INVALID  "); got != "example.invalid" {
		t.Errorf("normalizeDomain = %q, want %q", got, "example.invalid")
	}
}

// TestGCReportsUnknownSubtree is the RO5X-034 regression: a storage_root child
// that is not a valid tenant is skipped by Walk, so its blobs are never
// marked, swept, or permission-checked — and the store silently grows. It must
// at least be reported, and must never be auto-deleted.
func TestGCReportsUnknownSubtree(t *testing.T) {
	root := t.TempDir()
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A subtree whose name is not a valid tenant (uppercase).
	stray := filepath.Join(root, "JDoe", "raw", "2026", "01", "01", "aa", "bb")
	if err := os.MkdirAll(stray, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	strayFile := filepath.Join(stray, strings.Repeat("a", 64)+".eml")
	if err := os.WriteFile(strayFile, []byte("x"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	var seen []string
	store.SetOnUnknownSubtree(func(name string) { seen = append(seen, name) })

	if err := store.Walk(blob.KindRaw, func(_ blob.Kind, _ blob.Tenant, _ blob.Bucket, _, _ string, _ os.FileInfo) error {
		t.Error("Walk visited a file under a non-tenant subtree")
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	var found bool
	for _, n := range seen {
		if n == "JDoe" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown subtree %q was not reported; seen = %v", "JDoe", seen)
	}

	// Never auto-delete: it could be an operator's staging area.
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("the file under the unrecognized subtree was touched: %v", err)
	}
}

// TestGCUnknownSubtreeCallbackIsOptional keeps the nil case safe.
func TestGCUnknownSubtreeCallbackIsOptional(t *testing.T) {
	root := t.TempDir()
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "NotATenant"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No callback set — must not panic.
	if err := store.Walk(blob.KindRaw, func(blob.Kind, blob.Tenant, blob.Bucket, string, string, os.FileInfo) error {
		return nil
	}); err != nil {
		t.Fatalf("Walk with no callback: %v", err)
	}
}
