package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// PostDeliveryHookContext carries the delivery facts a hook may want.
// All fields are populated only AFTER the PG transaction has committed,
// so the hook can safely assume the message is durable.
type PostDeliveryHookContext struct {
	EnvelopeFrom string
	EnvelopeTo   string
	MailboxID    int64
	FolderID     int64
	UID          int64
	MessageID    int64
	ModSeq       int64
	RawSHA256Hex string
	RawSize      int64
	Attachments  int
	IsCatchall   bool
}

// Post-delivery hooks run ONLY in `deliver`, a short-lived per-message process,
// while /metrics is served by the long-running `serve` process — so any
// Prometheus counters registered here would never be scraped. Hook
// observability is therefore log-only: the slog lines below carry outcome +
// elapsed_ms, which log aggregation derives metrics from. Per-delivery
// Prometheus metrics would need a pushgateway/textfile collector (out of
// scope) — R-049.

// runPostDeliveryHook executes the configured shell hook (if any). Hook
// failures are LOGGED, NEVER PROPAGATED — the message is already durably
// committed at this point and re-running the LDA would duplicate it.
//
// The hook runs with its own deadline (cfg.Delivery.PostHookTimeout) and
// inherits the LDA's stdout/stderr so its diagnostics land in the mail log.
// Stdin is closed; if the hook reads stdin it gets EOF immediately.
func runPostDeliveryHook(cfg *Config, info PostDeliveryHookContext) {
	if len(cfg.Delivery.PostHookCommand) == 0 {
		return
	}

	timeout := cfg.PostHookTimeoutDuration()
	// Decouple from the per-delivery context: that context's deadline may
	// have already lapsed by the time we get here (especially if Ingest
	// itself was slow). The hook gets its own clock.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := cfg.Delivery.PostHookCommand
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(),
		"MAIL_DB_ENVELOPE_FROM="+info.EnvelopeFrom,
		"MAIL_DB_ENVELOPE_TO="+info.EnvelopeTo,
		"MAIL_DB_MAILBOX_ID="+strconv.FormatInt(info.MailboxID, 10),
		"MAIL_DB_FOLDER_ID="+strconv.FormatInt(info.FolderID, 10),
		"MAIL_DB_UID="+strconv.FormatInt(info.UID, 10),
		"MAIL_DB_MESSAGE_ID="+strconv.FormatInt(info.MessageID, 10),
		"MAIL_DB_MODSEQ="+strconv.FormatInt(info.ModSeq, 10),
		"MAIL_DB_RAW_SHA256="+info.RawSHA256Hex,
		"MAIL_DB_RAW_BYTES="+strconv.FormatInt(info.RawSize, 10),
		"MAIL_DB_ATTACHMENTS="+strconv.Itoa(info.Attachments),
		"MAIL_DB_IS_CATCHALL="+strconv.FormatBool(info.IsCatchall),
	)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	switch {
	case err == nil:
		slog.Info("post-delivery hook ok",
			"command", args[0],
			"elapsed_ms", elapsed.Milliseconds(),
			"message_id", info.MessageID,
		)
	case isHookTimeout(ctx, err):
		slog.Warn("post-delivery hook timed out",
			"command", args[0],
			"timeout", timeout,
			"message_id", info.MessageID,
		)
	default:
		// exec.ExitError carries the non-zero exit code; log it for
		// quick triage. Anything else (fork failed, command missing)
		// surfaces as the raw error.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			slog.Warn("post-delivery hook non-zero exit",
				"command", args[0],
				"exit_code", ee.ExitCode(),
				"elapsed_ms", elapsed.Milliseconds(),
				"message_id", info.MessageID,
			)
			return
		}
		slog.Warn("post-delivery hook error",
			"command", args[0],
			"err", err,
			"elapsed_ms", elapsed.Milliseconds(),
			"message_id", info.MessageID,
		)
	}
}

// isHookTimeout reports whether err is the result of the context deadline
// firing rather than an exec-level failure. exec.CommandContext kills the
// child when the deadline fires, and the resulting *exec.ExitError
// ("signal: killed") never wraps context.DeadlineExceeded — ctx.Err() is
// the authoritative signal.
func isHookTimeout(ctx context.Context, _ error) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}
