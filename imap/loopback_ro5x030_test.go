package main

import (
	"testing"

	"github.com/ptudor/epistula-mail/database/netutil"
)

// TestIsLoopbackAddrMatchesCanonical is the RO5X-030 anti-divergence test.
//
// "Is this loopback?" had seven implementations across five projects and they
// disagreed — one compared "localhost" case-sensitively, one treated an
// unparseable address as a bare host. These are security-relevant predicates
// (they gate whether an unauthenticated endpoint may bind a reachable
// interface), so a fix applied to one copy that is not applied to the others
// is the actual hazard. This project's helper must answer exactly as
// epistula-database/netutil does.
func TestIsLoopbackAddrMatchesCanonical(t *testing.T) {
	for _, tc := range netutil.AddrCases {
		if got := isLoopbackAddr(tc.Addr); got != tc.Want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v (canonical: %v)",
				tc.Addr, got, tc.Want, netutil.IsLoopbackAddr(tc.Addr))
		}
	}
}
