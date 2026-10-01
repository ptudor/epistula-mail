package ingest

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// decodeText converts a byte slice to a UTF-8 string using the given MIME
// charset name. UTF-8 and US-ASCII pass through. Unknown charsets degrade to
// the raw bytes (invalid UTF-8 may result; the caller's tsvector index uses
// 'simple' which tolerates).
func decodeText(b []byte, charset string) (string, error) {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "us-ascii" || cs == "ascii" {
		return string(b), nil
	}
	enc, err := lookupEncoding(cs)
	if err != nil || enc == nil {
		// Last-ditch: if the bytes are already valid UTF-8, return them; else
		// pass through and let the caller decide.
		if utf8.Valid(b) {
			return string(b), nil
		}
		return string(b), err
	}
	converted, _, terr := transform.Bytes(enc.NewDecoder(), b)
	if terr != nil {
		return string(b), terr
	}
	return string(converted), nil
}

func lookupEncoding(charset string) (encoding.Encoding, error) {
	if enc, err := ianaindex.MIME.Encoding(charset); err == nil && enc != nil {
		return enc, nil
	}
	if enc, err := ianaindex.IANA.Encoding(charset); err == nil && enc != nil {
		return enc, nil
	}
	return nil, nil
}

// charsetReader is the mime.WordDecoder hook for non-UTF-8 encoded-words.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := lookupEncoding(charset)
	if err != nil || enc == nil {
		return input, nil
	}
	return transform.NewReader(input, enc.NewDecoder()), nil
}

// htmlToText extracts a plain-text projection from HTML for FTS and for
// plaintext-only IMAP clients. Drops <script> / <style> content; inserts
// newlines for block-level closers. The output is not pretty, but it indexes
// well and is human-readable.
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var buf bytes.Buffer
	dropDepth := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			if dropDepth == 0 {
				buf.Write(z.Text())
			}
		case html.StartTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				dropDepth++
			case "br":
				if dropDepth == 0 {
					buf.WriteByte('\n')
				}
			case "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				if dropDepth == 0 {
					buf.WriteByte('\n')
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				if dropDepth > 0 {
					dropDepth--
				}
			case "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				if dropDepth == 0 {
					buf.WriteByte('\n')
				}
			}
		case html.SelfClosingTagToken:
			name, _ := z.TagName()
			if string(name) == "br" && dropDepth == 0 {
				buf.WriteByte('\n')
			}
		}
	}
	return collapseWhitespace(buf.String())
}

// collapseWhitespace squashes runs of spaces/tabs/CR into a single space,
// preserves newlines, and trims leading/trailing whitespace.
func collapseWhitespace(s string) string {
	var out strings.Builder
	prevSpace := false
	for _, r := range s {
		switch r {
		case '\n':
			out.WriteRune('\n')
			prevSpace = false
		case ' ', '\t', '\r':
			if !prevSpace {
				out.WriteByte(' ')
			}
			prevSpace = true
		default:
			out.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(out.String())
}
