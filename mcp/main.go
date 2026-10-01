// epistula-mcp — the mail store exposed to an MCP client (Claude Desktop / Claude
// Code / the claude.ai app, or any other MCP host) so a human or a model can ask
// questions about the mail store: "what came in this week?", "find the invoice
// from the hosting company", "which messages did the classifier tag receipts?".
//
// It is a THIN PROXY over epistula-api's /v1 HTTP contract — no database driver, no
// schema import, no LLM calls. Every tool call becomes one HTTP request to
// epistula-api with a bearer token, and the response is slimmed to the fields that
// answer the question. All the real logic (scope enforcement, FTS ranking,
// annotation priority, cursor pagination, rate limiting) stays in epistula-api.
//
// Two transports:
//
//   - stdio (default): the MCP client launches this as a subprocess and speaks
//     JSON-RPC over stdin/stdout. The normal, per-client mode — run it on a
//     workstation with base_url pointing at the Apache-fronted https://
//     epistula-api, or on the epistula-api host itself against
//     http://127.0.0.1:8784.
//
//   - Streamable HTTP (-http <addr>): a long-running daemon for the rc.d/launchd
//     case, or to give a remote MCP host one endpoint. The transport has no auth
//     of its own, so a bearer token (http_token) gates it, and the daemon refuses
//     to bind a non-loopback address without one — you cannot accidentally expose
//     an endpoint that holds a mail-reading credential.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// httpDrainTimeout is how long a SIGTERM waits for in-flight tool calls to
// finish before the remaining connections are dropped. It is the advertised
// graceful-drain window, and since RA6X-045 it is actually honoured: main waits
// for it rather than returning as soon as the listener closes.
const httpDrainTimeout = 10 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("epistula-mcp: ")

	httpAddr := flag.String("http", "",
		"serve over Streamable HTTP on this address (e.g. 127.0.0.1:8786) instead of stdio")
	configPath := flag.String("config", "",
		"path to a TOML config file (optional; default "+defaultConfigPath+" if it exists)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("epistula-mcp %s (built %s)\n", Version, BuildTime)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	readClient := newClient(cfg)

	// The annotate tool exists only when a second, write-scoped credential was
	// supplied. Default (no annotate_token) is read-only: six tools, and no
	// write capability for a model reading attacker-supplied mail to reach.
	var writeClient *client
	if cfg.AnnotateToken != "" {
		writeClient = newClientWithToken(cfg, cfg.AnnotateToken)
		log.Print("annotate tool enabled (separate write-scoped epistula-api token)")
	} else {
		log.Print("read-only: no annotate_token, annotate tool not registered")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "mail", Version: Version}, nil)
	(&toolset{c: readClient, w: writeClient, cfg: cfg}).register(server)

	if *httpAddr != "" {
		var handler http.Handler = mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return server }, nil)
		switch {
		case cfg.HTTPToken != "":
			handler = bearerAuth(cfg.HTTPToken, handler)
			log.Print("HTTP transport: bearer token required")
		case isLoopbackAddr(*httpAddr):
			log.Print("HTTP transport: no token (loopback only)")
		default:
			// Never expose an unauthenticated endpoint that carries a
			// mail-reading credential on a reachable interface by accident —
			// fail loud instead.
			log.Fatalf("refusing to bind non-loopback address %s without http_token "+
				"(anyone who could reach it could read mail through the epistula-api token); "+
				"set http_token (or MAIL_MCP_HTTP_TOKEN), or bind to loopback", *httpAddr)
		}
		// http.ListenAndServe uses a zero-value http.Server: no read,
		// header, or idle deadline at all. A peer that opens a connection
		// and dribbles header bytes would hold a goroutine and an fd
		// forever — a trivial slowloris against an endpoint that carries a
		// mail-reading credential, and one reachable from the network in the
		// intended deployment (the guard above permits a non-loopback bind
		// as soon as http_token is set). Set them, as every other daemon in
		// this stack does (RO5X-017).
		srv := &http.Server{
			Addr:              *httpAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second, // tool-call bodies are small JSON
			IdleTimeout:       120 * time.Second,
			// WriteTimeout is deliberately 0: the MCP Streamable HTTP
			// transport holds a response open for server-sent events, and a
			// write deadline would cut live sessions. epistula-api documents the
			// same reasoning for /v1/export.
		}

		// Drain gracefully on SIGINT/SIGTERM.
		shutdownCtx, stop := signal.NotifyContext(context.Background(),
			os.Interrupt, syscall.SIGTERM)
		defer stop()

		log.Printf("Streamable HTTP on %s", *httpAddr)
		if err := serveWithDrain(srv, shutdownCtx, stop, httpDrainTimeout); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		// A clean client disconnect on stdio surfaces as EOF/closed-pipe or a
		// cancelled context; don't treat it as a crash.
		if err != context.Canceled {
			fmt.Fprintln(os.Stderr, "epistula-mcp:", err)
		}
	}
}

// serveWithDrain runs srv until shutdownCtx fires, then waits for the graceful
// drain to finish before returning (RA6X-045).
//
// Waiting is the whole point. http.Server.Shutdown closes the listener FIRST,
// so ListenAndServe returns ErrServerClosed almost immediately while Shutdown
// is still waiting for in-flight handlers. The caller used to return at that
// moment, and a Go process ends when main returns regardless of what its
// goroutines are doing — so a tool call in progress, an annotation write
// included, was cut off despite the advertised ten-second drain window.
//
// stop is the signal-notify cancel. It is called as soon as the listener
// returns, whichever way it ended, so an UNEXPECTED listener failure (a port
// already taken, an fd limit) does not leave the drain goroutine parked on a
// signal that will never arrive with this function waiting on it forever.
//
// A handler that never returns cannot be waited for indefinitely either:
// Shutdown reports the deadline, Close then drops what is left, and the process
// can exit. That is a bounded hard stop, not a hang.
func serveWithDrain(srv *http.Server, shutdownCtx context.Context, stop func(), drainTimeout time.Duration) error {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-shutdownCtx.Done()
		log.Print("shutting down HTTP transport")
		drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
			_ = srv.Close()
		}
	}()

	serveErr := srv.ListenAndServe()
	stop()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		// Not a shutdown: the listener itself failed. Tear the server down so
		// the drain goroutine finishes, then report it.
		_ = srv.Close()
		<-drained
		return serveErr
	}
	<-drained
	return nil
}
