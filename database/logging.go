package main

import (
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
)

func setupLogging(cfg *Config) {
	// AddSource stays explicitly false per the CLAUDE.md logging contract:
	// no file:line leakage to log aggregation.
	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.Logging.Level), AddSource: false}
	var handler slog.Handler
	if strings.EqualFold(cfg.Logging.Format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redactedHeaderPattern matches header names whose values must never be
// logged. Deliberately unanchored, per the CLAUDE.md contract: any header
// merely CONTAINING one of these words is redacted (X-Auth-Token,
// Proxy-Authorization, X-Amz-Security-Token, X-Encryption-Key, ...).
// Over-redaction is the safe failure mode.
var redactedHeaderPattern = regexp.MustCompile(`(?i)(authorization|cookie|password|secret|token|key)`)

// RedactHeader returns the header value or "<redacted>" if the name matches a
// secrets pattern. Use whenever a log line includes raw header data.
func RedactHeader(name, value string) string {
	if redactedHeaderPattern.MatchString(name) {
		return "<redacted>"
	}
	return value
}

// LoggableHeaders wraps a header map so it is redacted BY CONSTRUCTION when
// logged.
//
// RedactHeader implements the CLAUDE.md redaction contract but was called from
// nowhere, so the contract held only by accident — the first
// `slog.Info("...", "headers", msg.Headers)` added anywhere would have
// violated it silently. A slog.LogValuer wrapper makes the safe form the easy
// form: log `LoggableHeaders(h)` and every matching value is masked, whatever
// the caller knew (RO5X-023).
//
// Keys are emitted in sorted order so two log lines for the same headers are
// byte-identical.
type LoggableHeaders map[string][]string

// LogValue implements slog.LogValuer.
func (h LoggableHeaders) LogValue() slog.Value {
	if len(h) == 0 {
		return slog.GroupValue()
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)

	attrs := make([]slog.Attr, 0, len(names))
	for _, name := range names {
		vals := h[name]
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = RedactHeader(name, v)
		}
		attrs = append(attrs, slog.String(name, strings.Join(out, ", ")))
	}
	return slog.GroupValue(attrs...)
}
