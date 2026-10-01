package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Opaque cursor codec. A cursor is base64url(JSON{v:1,k:[...]}) over the
// ordered key values of the last row served — (uid) for folder listings,
// (internal_date_unixmicro, id) for date-ordered listings. The encoding is
// deliberately content-free: a tampered cursor only repositions the caller
// within data its token scope already permits (every query re-applies the
// scope filter), so integrity protection would add nothing.

var errBadCursor = errors.New("malformed cursor")

type cursorPayload struct {
	V int     `json:"v"`
	K []int64 `json:"k"`
}

func encodeCursor(keys ...int64) string {
	b, _ := json.Marshal(cursorPayload{V: 1, K: keys})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a cursor and enforces the expected key arity.
func decodeCursor(s string, want int) ([]int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errBadCursor
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errBadCursor
	}
	if p.V != 1 || len(p.K) != want {
		return nil, fmt.Errorf("%w: version or arity mismatch", errBadCursor)
	}
	return p.K, nil
}
