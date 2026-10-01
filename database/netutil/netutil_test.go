package netutil

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	for _, tc := range AddrCases {
		if got := IsLoopbackAddr(tc.Addr); got != tc.Want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tc.Addr, got, tc.Want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, tc := range HostCases {
		if got := IsLoopbackHost(tc.Host); got != tc.Want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", tc.Host, got, tc.Want)
		}
	}
}
