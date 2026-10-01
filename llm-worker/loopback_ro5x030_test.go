package main

import "testing"

// loopbackAddrCases mirrors epistula-database/netutil's exported AddrCases.
//
// This project deliberately does not depend on epistula-database (it is a pure
// consumer of epistula-api's HTTP contract), so the table is duplicated rather
// than imported. Keep it byte-identical to netutil's — that is the whole
// point of RO5X-030: seven implementations that disagreed with each other.
var loopbackAddrCases = []struct {
	addr string
	want bool
}{
	{"127.0.0.1:993", true},
	{"127.0.0.1:0", true},
	{"127.5.6.7:8080", true},
	{"[::1]:993", true},
	{"localhost:993", true},
	{"LOCALHOST:993", true},
	{"LocalHost:8080", true},

	{":993", false},
	{"0.0.0.0:993", false},
	{"[::]:993", false},
	{"192.0.2.1:993", false},
	{"example.invalid:993", false},
	{"garbage", false},
	{"", false},
	{"127.0.0.1", false},
}

var loopbackHostCases = []struct {
	host string
	want bool
}{
	{"127.0.0.1", true},
	{"127.255.255.254", true},
	{"::1", true},
	{"localhost", true},
	{"LOCALHOST", true},
	{"LocalHost", true},

	{"0.0.0.0", false},
	{"::", false},
	{"192.0.2.1", false},
	{"example.invalid", false},
	{"", false},
	{"garbage", false},
}

func TestIsLoopbackListenMatchesCanonicalTable(t *testing.T) {
	for _, tc := range loopbackAddrCases {
		if got := isLoopbackListen(tc.addr); got != tc.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestIsLoopbackHostMatchesCanonicalTable(t *testing.T) {
	for _, tc := range loopbackHostCases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
