package ingest

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func projectionFixture() ([]byte, Limits) {
	limits := DefaultLimits()
	limits.MaxMessageBytes = 1 << 20
	header := []byte("Content-Type: text/plain; charset=utf-16le\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	raw := append(header, bytes.Repeat([]byte{0x2d, 0x4e}, (int(limits.MaxMessageBytes)-len(header))/2)...)
	return raw, limits
}
func TestCharsetProjectionIsBounded(t *testing.T) {
	raw, limits := projectionFixture()
	message, err := New(limits).Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.TextBody) > int(limits.MaxMessageBytes) || !utf8.ValidString(message.TextBody) {
		t.Fatalf("projection len=%d valid=%v", len(message.TextBody), utf8.ValidString(message.TextBody))
	}
	found := false
	for _, d := range message.Defects {
		if strings.HasSuffix(d, DefectProjectionTruncated) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing projection defect: %v", message.Defects)
	}
	if !bytes.Equal(message.Raw, raw) {
		t.Fatal("raw message changed")
	}
}
func BenchmarkParseCharsetProjection(b *testing.B) {
	raw, limits := projectionFixture()
	parser := New(limits)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parser.Parse(raw); err != nil {
			b.Fatal(err)
		}
	}
}
