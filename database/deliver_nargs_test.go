package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ptudor/epistula-mail/database/ingest"
)

// TestRunDeliverRejectsTrailingArgs is the R-003 guard: if Postfix expands
// ${recipient} to multiple argv words (missing recipient_limit=1), the extra
// addresses land in fs.Args() and would be silently dropped. runDeliver must
// return EX_TEMPFAIL (defer) rather than proceed with only the first — never a
// bounce-class code, so no recipient's copy is lost.
func TestRunDeliverRejectsTrailingArgs(t *testing.T) {
	code := runDeliver([]string{"-recipient=a@example.invalid", "b@example.invalid"})
	if code != EX_TEMPFAIL {
		t.Fatalf("runDeliver with trailing args returned %d, want EX_TEMPFAIL (%d)", code, EX_TEMPFAIL)
	}
}

// TestRunDeliverMissingRecipient is the RO5X-035 regression.
//
// This used to assert EX_USAGE. But EX_USAGE is a PERMANENT failure to Postfix
// (5.x.x), so an empty ${recipient} — an empty envelope recipient, or a
// mangled argv= after a master.cf edit — DESTROYED the message instead of
// requeueing it. That is exactly the outcome the sibling trailing-args guard
// above works to avoid for the same class of misconfiguration.
//
// Every argv-level problem in the deliver subcommand now defers. (The other
// subcommands' EX_USAGE returns are read by a human shell and stay as they
// are.)
func TestRunDeliverMissingRecipient(t *testing.T) {
	code := runDeliver([]string{})
	if code != EX_TEMPFAIL {
		t.Fatalf("runDeliver with no -recipient returned %d, want EX_TEMPFAIL (%d) — "+
			"a permanent code here loses mail", code, EX_TEMPFAIL)
	}
	if !IsTemporaryFailure(code) {
		t.Errorf("IsTemporaryFailure(%d) = false; Postfix must requeue, not bounce", code)
	}
	if IsPermanentFailure(code) {
		t.Errorf("IsPermanentFailure(%d) = true; this must never bounce", code)
	}
}

// TestParseErrorToExitClassifies is the RO5X-025 regression: the switch used
// to return EX_DATAERR from both arms, so its default silently bounced any
// error that was not a data error. The named sentinels still bounce; anything
// unrecognized now defers.
func TestParseErrorToExitClassifies(t *testing.T) {
	for _, sentinel := range []error{
		ingest.ErrTooLarge, ingest.ErrTooDeep, ingest.ErrTooManyParts,
		ingest.ErrHeaderTooLarge, ingest.ErrHeadersTooBig, ingest.ErrZipBomb,
		ingest.ErrMalformed,
	} {
		if got := parseErrorToExit(sentinel); got != EX_DATAERR {
			t.Errorf("parseErrorToExit(%v) = %d, want EX_DATAERR (%d)", sentinel, got, EX_DATAERR)
		}
	}
	// A wrapped sentinel still classifies as a data error.
	if got := parseErrorToExit(fmt.Errorf("parsing: %w", ingest.ErrMalformed)); got != EX_DATAERR {
		t.Errorf("wrapped sentinel = %d, want EX_DATAERR (%d)", got, EX_DATAERR)
	}
	// Anything else defers rather than bouncing.
	if got := parseErrorToExit(errors.New("boom")); got != EX_TEMPFAIL {
		t.Errorf("parseErrorToExit(unknown) = %d, want EX_TEMPFAIL (%d) — "+
			"an unclassified error must defer, not bounce", got, EX_TEMPFAIL)
	}
}
