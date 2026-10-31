package main

// Version and BuildTime are injected at build time via -ldflags (see Makefile):
//
//	-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)
//
// The defaults identify an ad-hoc `go build` binary. This follows the repo
// convention (epistula-api, epistula-database) of package-main ldflags vars.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// projectURL is the comment every User-Agent carries, so an operator reading
// a mail log or an epistula-api access log can find the software.
const projectURL = "https://github.com/ptudor/epistula-mail"

// userAgent names this connector and build: the User-Agent of every
// epistula-api request, and the field the send tool adds to a message that
// does not name its own composer.
func userAgent() string {
	return "epistula-mcp/" + Version + " (+" + projectURL + ")"
}
