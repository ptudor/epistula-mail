package main

// Exit codes for the epistula-api daemon. Not a Postfix-invoked binary, so only
// the subset of sysexits the daemon actually returns is defined.
const (
	EX_OK     = 0
	EX_USAGE  = 64
	EX_CONFIG = 78
	EX_OSERR  = 71
)
