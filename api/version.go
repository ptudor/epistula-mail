package main

// Version and BuildTime are injected at build time via -ldflags (see
// Makefile). The defaults identify ad-hoc `go build` binaries.
var (
	Version   = "dev"
	BuildTime = "unknown"
)
