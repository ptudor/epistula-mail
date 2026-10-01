// Conformance tables for the loopback predicates.
//
// These live in the package (not in _test.go) so the other Epistula components that
// import netutil can assert their own wrappers against the SAME table. That is
// the entire point of RO5X-030: the predicate had seven implementations that
// disagreed with one another, and a shared table is what stops them drifting
// apart again.
//
// Projects that cannot import netutil — epistula-mcp and epistula-llm-worker, which
// depend only on epistula-api's HTTP contract — duplicate the table verbatim in
// their own tests and are held to it there.

package netutil

// AddrCases is the shared truth table for IsLoopbackAddr. It is exported so
// every project's local wrapper — including epistula-mcp's, which cannot import
// this package's implementation but can be held to the same table — asserts
// the identical answers (RO5X-030).
var AddrCases = []struct {
	Addr string
	Want bool
}{
	{"127.0.0.1:993", true},
	{"127.0.0.1:0", true},
	{"127.5.6.7:8080", true}, // all of 127.0.0.0/8
	{"[::1]:993", true},
	{"localhost:993", true},
	{"LOCALHOST:993", true}, // DNS names are case-insensitive
	{"LocalHost:8080", true},

	{":993", false},        // wildcard: every interface
	{"0.0.0.0:993", false}, // wildcard
	{"[::]:993", false},    // wildcard
	{"192.0.2.1:993", false},
	{"example.invalid:993", false},
	{"garbage", false}, // no port: not a listen address
	{"", false},
	{"127.0.0.1", false}, // no port
}

// HostCases is the shared truth table for IsLoopbackHost.
var HostCases = []struct {
	Host string
	Want bool
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
