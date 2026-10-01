// Package netutil holds the network predicates the stack shares.
//
// It exists because "is this address loopback?" had seven implementations
// across the five projects and they disagreed with each other (RO5X-030).
// These are security-relevant predicates — they gate whether an
// unauthenticated metrics/admin/MCP endpoint may bind a reachable interface —
// so a divergence means a fix applied to one copy is not applied to the
// others.
//
// `epistula-mcp` cannot import this by design (it depends only on epistula-api's /v1
// HTTP contract, with no `replace` directive and no epistula-database import), so
// it keeps a single local copy pointing back here, held to the same truth
// table by a shared test vector.
package netutil

import (
	"net"
	"strings"
)

// IsLoopbackHost reports whether a bare hostname or IP literal refers to
// loopback.
//
// "localhost" matches case-insensitively — DNS names are case-insensitive, and
// one of the pre-existing copies compared it case-sensitively, so
// `LOCALHOST:993` was treated as non-loopback there and as loopback in the
// other five.
//
// Everything else must parse as an IP in 127.0.0.0/8 or ::1. An empty host, a
// wildcard bind ("0.0.0.0", "::"), and any unparseable string are NOT
// loopback — a wildcard binds every interface, which is the opposite of what
// callers are asking about.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackAddr reports whether a "host:port" listen address is loopback.
//
// An address that does not split into host and port is NOT loopback. This is
// the deliberate, fail-closed reading: a bare ":993" or a malformed value must
// never be mistaken for a safe bind, and `http.Server{Addr: ""}` listens on
// every interface. (One pre-existing copy fell back to treating the whole
// string as a host on a split failure, which classified some malformed values
// differently from the other six.)
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return IsLoopbackHost(host)
}
