package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// decodeJSONBody reads exactly one JSON object from the request body into dst,
// refusing unknown fields, a body over maxBytes, and anything after the first
// value. It writes the problem response and returns false on failure. Both
// write endpoints (annotation, classification) decode through it.
//
// Exactly one value, then nothing but whitespace (RA6X-041): json.Decoder
// reads ONE value and stops, so a body of `{...}{...}` would commit the first
// object and silently discard the second — a client that believed it had sent
// two writes, or a proxy that concatenated a retry onto the original, would
// get a 204 for work that was half done. Reading the tail through the same
// MaxBytesReader is what makes the cap apply to the WHOLE body rather than to
// the prefix that happened to parse.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problemUnprocessable(w, r, fmt.Sprintf("body exceeds %d bytes", maxBytes))
			return false
		}
		problemBadRequest(w, r, "Malformed JSON body: "+err.Error())
		return false
	}
	var trailing json.RawMessage
	switch err := dec.Decode(&trailing); {
	case errors.Is(err, io.EOF):
		return true
	case err == nil:
		problemBadRequest(w, r, "Body must contain exactly one JSON object; a second value follows it.")
		return false
	default:
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problemUnprocessable(w, r, fmt.Sprintf("body exceeds %d bytes", maxBytes))
			return false
		}
		problemBadRequest(w, r, "Body must contain exactly one JSON object; trailing data follows it.")
		return false
	}
}
