// Bearer-token auth for the -http (Streamable HTTP) transport.
//
// The SDK's Streamable HTTP transport has no authentication of its own — a
// reachable port would act with the epistula-api token's full (scoped) privileges.
// On loopback that's acceptable; the moment the daemon binds a LAN/public address
// it needs a gate. bearerAuth is that gate: every request must carry
// `Authorization: Bearer <http_token>` or gets a 401. stdio needs no token (it's
// a local subprocess), so this wraps only the HTTP handler. For anything public,
// TLS still goes in front (Apache, as everywhere else in this stack).
package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// bearerAuth wraps next, requiring a matching bearer token on every request. The
// token is compared in constant time so a wrong guess leaks nothing via timing.
func bearerAuth(token string, next http.Handler) http.Handler {
	want := []byte(token)
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) &&
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// isLoopbackAddr reports whether a "host:port" listen address binds loopback
// only. The host is judged by isLoopbackHost in config.go, the single local
// copy of epistula-database/netutil's rule (RO5X-030): "localhost" in any case
// or a loopback IP. A wildcard host (empty, 0.0.0.0, ::) binds every interface
// and is NON-loopback, so the token guard trips on ":8786" just as it does on
// a specific LAN IP.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Fail closed: a value that is not host:port (no port, unbalanced
		// brackets) must never be mistaken for a safe bind.
		return false
	}
	return isLoopbackHost(host)
}
