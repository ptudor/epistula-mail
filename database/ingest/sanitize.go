package ingest

import (
	"strings"
	"unicode/utf8"
)

// SanitizeUTF8 coerces s into a string Postgres TEXT/JSONB columns accept:
// invalid UTF-8 sequences become U+FFFD and NUL bytes are stripped (Postgres
// rejects the NUL escape even inside otherwise-valid JSONB). Real-world mail
// carries un-encoded 8-bit headers and mislabeled charsets constantly;
// rejecting it would bounce routine messages, and storing it raw would fail
// the INSERT mid-delivery. The raw wire bytes on disk are never touched —
// this applies only to the derived columns.
func SanitizeUTF8(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "")
}

// sanitizeParams returns params with every key and value passed through
// SanitizeUTF8. The map is JSONB-bound (BODYSTRUCTURE params); a nil or
// clean map is returned as-is.
func sanitizeParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	clean := true
	for k, v := range params {
		if SanitizeUTF8(k) != k || SanitizeUTF8(v) != v {
			clean = false
			break
		}
	}
	if clean {
		return params
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[SanitizeUTF8(k)] = SanitizeUTF8(v)
	}
	return out
}
