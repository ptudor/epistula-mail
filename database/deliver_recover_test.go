package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// captureSlog installs a JSON slog handler writing to buf for the duration of
// fn, restoring the previous default logger afterwards. Returns the decoded
// records so a test can assert on individual log-line fields.
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	fn()

	var recs []map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode slog record: %v", err)
		}
		recs = append(recs, m)
	}
	return recs
}

// TestRecoverDeliverPanicYieldsTempfail is the R-001 regression guard: a
// panic inside the delivery body must return EX_TEMPFAIL (requeue), never the
// zero value EX_OK (which Postfix treats as delivered and deletes the queue
// file). The bug was a shadowing unnamed result; go vet does not catch it.
func TestRecoverDeliverPanicYieldsTempfail(t *testing.T) {
	var code int
	recs := captureSlog(t, func() {
		code = recoverDeliver("victim@example.invalid", &deliveryAcceptance{}, func() int {
			panic("simulated pgx internal panic mid-delivery")
		})
	})
	if code != EX_TEMPFAIL {
		t.Fatalf("panic body returned %d, want EX_TEMPFAIL (%d)", code, EX_TEMPFAIL)
	}

	// The terminal "deliver exit" log line's code field must match the code
	// actually returned — the old bug logged code=75 while exiting 0.
	var exitRec map[string]any
	for _, r := range recs {
		if r["msg"] == "deliver exit" {
			exitRec = r
		}
	}
	if exitRec == nil {
		t.Fatal("no \"deliver exit\" log line emitted on panic path")
	}
	if got := exitRec["code"]; got != float64(EX_TEMPFAIL) {
		t.Fatalf("deliver exit log code=%v, want %d", got, EX_TEMPFAIL)
	}
	if got := exitRec["disposition"]; got != "requeue" {
		t.Fatalf("deliver exit disposition=%v, want requeue", got)
	}
}

// TestRecoverDeliverPassesThroughNonPanicCodes verifies the helper does not
// alter a normal (non-panic) return — every sysexits code the body returns is
// returned verbatim, and the log line reports it.
func TestRecoverDeliverPassesThroughNonPanicCodes(t *testing.T) {
	for _, want := range []int{EX_OK, EX_NOUSER, EX_DATAERR, EX_CONFIG, EX_TEMPFAIL} {
		want := want
		var code int
		recs := captureSlog(t, func() {
			code = recoverDeliver("rcpt@example.invalid", &deliveryAcceptance{}, func() int { return want })
		})
		if code != want {
			t.Fatalf("recoverDeliver returned %d, want %d", code, want)
		}
		var exitRec map[string]any
		for _, r := range recs {
			if r["msg"] == "deliver exit" {
				exitRec = r
			}
		}
		if exitRec == nil {
			t.Fatalf("code %d: no \"deliver exit\" log line", want)
		}
		if got := exitRec["code"]; got != float64(want) {
			t.Fatalf("code %d: log code=%v", want, got)
		}
	}
}
