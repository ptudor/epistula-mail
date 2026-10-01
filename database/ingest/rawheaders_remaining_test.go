package ingest

import (
	"errors"
	"strings"
	"testing"
)

func TestRemainingChildHeadersUseRawLimits(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		for _, field := range []string{"X: " + strings.Repeat(" ", 1000) + "a", "X: a" + nl + strings.Repeat(" ", 1000) + "b"} {
			raw := "Content-Type: multipart/mixed; boundary=b" + nl + nl + "--b" + nl + field + nl + nl + "body" + nl + "--b--" + nl
			lim := DefaultLimits()
			lim.MaxHeaderBytes = 100
			lim.MaxHeaderSectionBytes = 4096
			_, err := New(lim).Parse([]byte(raw))
			if !errors.Is(err, ErrHeaderTooLarge) {
				t.Fatalf("unfolding bypassed raw limit: %v", err)
			}
		}
	}
}

func TestRemainingHeaderPreflightDoesNotAllocate(t *testing.T) {
	raw := []byte("--b\r\nX: " + strings.Repeat(" ", 1<<20) + "v\r\n\r\nbody\r\n--b--\r\n")
	lim := Limits{MaxHeaderBytes: 100, MaxHeaderSectionBytes: 2 << 20}
	allocs := testing.AllocsPerRun(20, func() {
		err := VisitRawMultipart(raw, "b", func(part []byte) error { return checkHeaderSection(part, lim) })
		if !errors.Is(err, ErrHeaderTooLarge) {
			t.Fatalf("preflight=%v", err)
		}
	})
	if allocs > 1 {
		t.Fatalf("preflight allocated %.0f times before refusing the header", allocs)
	}
}

func TestRemainingHeaderBoundaryAndPartCaps(t *testing.T) {
	lim := Limits{MaxHeaderBytes: 10, MaxHeaderSectionBytes: 30}
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"X: 1234567\r\n\r\nbody", nil},
		{"X: 12345678\r\n\r\nbody", ErrHeaderTooLarge},
		{"X: 12345\n 67\n\nbody", ErrHeaderTooLarge},
		{"X: 1\r\nX: 2\r\nX: 3\r\nX: 4\r\nX: 5\r\nX: 6\r\n\r\nbody", ErrHeadersTooBig},
		{"\nbody\r\n\r\nlater", nil},
	} {
		if err := checkHeaderSection([]byte(tc.raw), lim); !errors.Is(err, tc.want) {
			t.Fatalf("%q: %v want %v", tc.raw, err, tc.want)
		}
	}
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" + strings.Repeat("--b\r\n\r\nx\r\n", 5) + "--b--\r\n"
	partLimits := DefaultLimits()
	partLimits.MaxMimeParts = 3
	if _, err := New(partLimits).Parse([]byte(raw)); !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("part cap=%v", err)
	}
}
