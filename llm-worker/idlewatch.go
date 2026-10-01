package main

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleWatch bounds how long an export stream may spend BLOCKED WAITING FOR
// INPUT (RA6X-014).
//
// The previous watchdog measured the time to obtain one complete decoded row —
// the network reads, the JSON decode, and the two Unmarshal passes over it.
// That is not idleness: a valid 33 MiB row takes longer than the budget to
// arrive and parse, so a stream that had delivered every byte was still
// aborted with "export stream idle for 5s with no row". The budget now covers
// exactly one blocked read: it is armed on entry to Read and disarmed on
// return, so decoding, unmarshalling and the caller's LLM inference are
// outside it by construction rather than by remembering to pause around each
// of them. A server that sends nothing still fires; a server that is sending
// never does, however slow the client is at consuming what it sent.
//
// The generation counter is the second half of the fix. time.Timer.Stop
// reports "already fired" without waiting for the callback to run, so a
// callback that had begun but not yet recorded itself could cancel a LATER
// phase — the stream torn down during the next row's inference for a stall
// that had already been handled. Every arm and disarm bumps gen under the
// mutex, and a callback that wakes holding a stale generation does nothing. A
// callback either takes the mutex before the bump, in which case it fires and
// is visible to every later Fired call, or it takes it after and is inert.
// There is no third outcome, and no window in which Fired can answer "no" for
// a callback that is about to cancel.
type idleWatch struct {
	budget time.Duration
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	gen   uint64
	fired bool
}

// newIdleWatch returns a disarmed watch. cancel is invoked once, from the timer
// goroutine, if a single read exceeds budget.
func newIdleWatch(budget time.Duration, cancel context.CancelFunc) *idleWatch {
	return &idleWatch{budget: budget, cancel: cancel}
}

// enter arms the budget for one read.
func (w *idleWatch) enter() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired {
		return
	}
	w.gen++
	gen := w.gen
	w.timer = time.AfterFunc(w.budget, func() {
		w.mu.Lock()
		stale := gen != w.gen
		if !stale {
			w.fired = true
			w.gen++ // no second callback for this generation can re-fire
		}
		w.mu.Unlock()
		if !stale {
			w.cancel()
		}
	})
}

// exit disarms the budget. Any callback still in flight for the generation
// being retired becomes inert.
func (w *idleWatch) exit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
}

// Fired reports whether a read exceeded the budget. Because enter and exit
// bump the generation under the same mutex, a true answer always refers to a
// stall that actually cancelled the stream.
func (w *idleWatch) Fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// Stop releases the timer for good.
func (w *idleWatch) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
}

// watchedReader runs each read under the watch's budget. It is the one place
// that decides what "waiting for the stream" means: time blocked in Read, and
// nothing else.
type watchedReader struct {
	r io.Reader
	w *idleWatch
}

func (p *watchedReader) Read(b []byte) (int, error) {
	p.w.enter()
	n, err := p.r.Read(b)
	p.w.exit()
	return n, err
}
