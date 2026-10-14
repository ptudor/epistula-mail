package ingest

import (
	"reflect"
	"testing"
)

func TestYearZeroDateIsUnusable(t *testing.T) {
	m := mustParse(t, []byte("Date: Mon, 01 Jan 0000 00:00:00 +0000\r\n\r\nbody\r\n"))
	if !m.SentDate.IsZero() || m.SentDateLocal != "" || m.Headers["Date"][0] != "Mon, 01 Jan 0000 00:00:00 +0000" {
		t.Fatalf("date fields: %v %q %v", m.SentDate, m.SentDateLocal, m.Headers)
	}
}

func TestTopLevelMissingBoundaryIsOpaque(t *testing.T) {
	m := mustParse(t, []byte("Content-Type: multipart/mixed\r\n\r\nopaque\r\n"))
	if m.BodyStructure.Type != "multipart" || len(m.BodyStructure.Parts) != 0 ||
		!reflect.DeepEqual(m.BodyStructure.Defects, []string{DefectMissingBoundary}) ||
		!reflect.DeepEqual(m.Defects, []string{"message: missing_boundary"}) {
		t.Fatalf("structure=%+v defects=%v", m.BodyStructure, m.Defects)
	}
}
