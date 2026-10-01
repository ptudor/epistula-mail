package main

// Exit codes for the epistula-imap daemon. The IMAP server is not invoked by
// Postfix, so the full sysexits set is not required — these are the values
// the daemon may return.
const (
	EX_OK     = 0
	EX_USAGE  = 64
	EX_CONFIG = 78
	EX_OSERR  = 71
)
