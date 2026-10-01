module github.com/ptudor/epistula-mail/mcp

go 1.26.0

// The official MCP Go SDK (stable v1.x, maintained with the Go team).
//
// This project is a THIN PROXY over epistula-api's /v1 HTTP contract. It deliberately
// has NO github.com/ptudor/epistula-mail/database import and NO replace
// directive — no schema coupling, no lockstep rebuilds. See CLAUDE.md.
require (
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/pelletier/go-toml/v2 v2.4.3
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
