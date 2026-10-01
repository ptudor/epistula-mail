package main

import (
	"log/slog"
	"os"
	"strings"
)

func setupLogging(cfg *Config) {
	// AddSource stays explicitly false per the project's logging contract:
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
