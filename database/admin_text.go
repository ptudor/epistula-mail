package main

import (
	"fmt"
	"strings"
)

func terminalSafe(s string) string {
	var out strings.Builder
	for _, r := range s {
		if (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x7f && r <= 0x9f) {
			fmt.Fprintf(&out, `\x%02x`, r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func validateLocalpart(s string) error {
	if s == "" {
		return fmt.Errorf("localpart is empty")
	}
	if len(s) > 64 {
		return fmt.Errorf("localpart exceeds 64 bytes")
	}
	for _, r := range s {
		if r == '@' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("localpart contains a forbidden character")
		}
	}
	return nil
}
func normalizeTokenName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("token name is empty")
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return "", fmt.Errorf("token name contains a control character")
		}
	}
	return s, nil
}
