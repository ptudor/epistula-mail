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
