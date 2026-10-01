package main

import "testing"

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(1748736000000000, 42)
	keys, err := decodeCursor(c, 2)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if keys[0] != 1748736000000000 || keys[1] != 42 {
		t.Errorf("keys = %v, want [1748736000000000 42]", keys)
	}
}

func TestCursorArityMismatch(t *testing.T) {
	c := encodeCursor(7)
	if _, err := decodeCursor(c, 2); err == nil {
		t.Fatal("arity mismatch must be rejected")
	}
}

func TestCursorMalformed(t *testing.T) {
	for _, c := range []string{"", "!!!", "bm90anNvbg", "eyJ2IjoyLCJrIjpbMV19"} {
		if _, err := decodeCursor(c, 1); err == nil {
			t.Errorf("decodeCursor(%q) should fail", c)
		}
	}
}
