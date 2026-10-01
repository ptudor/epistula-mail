package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirectHops bounds a same-origin redirect chain. Go's default is 10;
// a legitimate deployment needs one hop (Apache adding a trailing slash to a
// base path), so a much smaller budget still serves every real case while
// making a redirect loop fail fast instead of after ten authenticated
// round-trips.
const maxRedirectHops = 3

// sameOriginRedirect is the http.Client CheckRedirect policy for every client
// in this process that carries a bearer token or mail content (RA6X-039).
//
// Validating the configured base URL says nothing about where a redirect will
// send the request. Go's default policy follows redirects and — because it
// considers a same-host destination trusted — forwards the Authorization
// header across an HTTPS→HTTP downgrade, handing the token to a plaintext
// listener. A 307/308 additionally replays the original request body, so an
// annotation PUT or an inference POST carrying mail text can be resent to
// another origin even where Go does strip the credential. Stripping
// Authorization is therefore not a sufficient fix on its own: the body is
// sensitive too.
//
// The policy: follow a redirect only when its destination has the same origin
// as the original request — identical scheme, host and effective port. That
// keeps the one redirect real deployments produce (a reverse proxy
// canonicalising an approved base path) working, while a scheme downgrade, a
// different host, a different port or a sibling subdomain is refused before
// any header or body is sent.
//
// Errors name origins only. A redirect target is attacker-influenced and the
// request URL can carry query parameters, so neither is echoed in full, and
// the token and body never appear.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if !sameOrigin(origin, req.URL) {
		return fmt.Errorf("refusing redirect from %s to %s: a redirect must not leave the "+
			"configured origin (it would disclose the bearer token, and a 307/308 would "+
			"replay the request body)", originOf(origin), originOf(req.URL))
	}
	if len(via) > maxRedirectHops {
		return fmt.Errorf("refusing redirect: more than %d hops within %s",
			maxRedirectHops, originOf(origin))
	}
	return nil
}

// sameOrigin reports whether two URLs share a scheme, host and effective port.
func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(hostPort(a), hostPort(b))
}

// hostPort renders a URL's authority with the scheme's default port made
// explicit, so https://h and https://h:443 compare equal.
func hostPort(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return host + ":" + port
}

// originOf renders a URL as scheme://host:port for an error message, dropping
// path, query and any userinfo.
func originOf(u *url.URL) string {
	if u == nil {
		return "(none)"
	}
	return strings.ToLower(u.Scheme) + "://" + hostPort(u)
}
