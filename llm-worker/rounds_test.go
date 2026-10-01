package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestFastRoundReadsOnlyTheQueue: a fast round works on the queued messages
// alone, leaves unqueued work to the complete round, and ends by pruning with
// this model and its classification requirement, which empties the queue of
// what it finished.
func TestFastRoundReadsOnlyTheQueue(t *testing.T) {
	s := newFakeStore()
	s.queued[1] = true // jdoe, new
	s.queued[4] = true // asmith, new, no categories
	w := newClassifyWorker(t, s)

	stats, err := w.runRound(context.Background(), roundFast)
	if err != nil {
		t.Fatalf("fast round: %v", err)
	}
	if !s.annotated[1] || !s.annotated[4] || s.annotated[2] {
		t.Fatalf("annotated = %v; want the queued 1 and 4 only", s.annotated)
	}
	if s.classified[1] != "travel" || s.classified[3] != "" {
		t.Fatalf("classified = %v; want queued 1 only (3 is not queued)", s.classified)
	}
	if len(s.prunes) != 1 || s.prunes[0] != (PassPrune{Model: "lmstudio:test", RequireClassification: true}) {
		t.Fatalf("prunes = %+v; want one for lmstudio:test requiring classification", s.prunes)
	}
	if !stats.QueueKnown || stats.QueuePruned != 2 || stats.QueueRemaining != 0 || len(s.queued) != 0 {
		t.Fatalf("stats %+v, queue %v; want both markers cleared", stats, s.queued)
	}

	// The complete round still finds what nothing queued.
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("complete round: %v", err)
	}
	if !s.annotated[2] || s.classified[3] != "finance/banking" {
		t.Fatalf("after the complete round annotated=%v classified=%v; want 2 annotated and 3 classified",
			s.annotated, s.classified)
	}
}

// TestFastRoundDefersAFailingMessage: a message that fails on its own content
// stays queued, fast rounds leave it alone until its retry time instead of
// spending a model call on it every interval, and a complete round retries it
// regardless.
func TestFastRoundDefersAFailingMessage(t *testing.T) {
	s := newFakeStore()
	s.queued[1] = true
	s.garbled = true
	w := newClassifyWorker(t, s)
	clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }

	if _, err := w.runRound(context.Background(), roundFast); err != nil {
		t.Fatalf("first fast round: %v", err)
	}
	if s.lmCalls != 1 || !s.queued[1] {
		t.Fatalf("after a failure: %d model call(s), queued=%v; want 1 call and the message still queued", s.lmCalls, s.queued)
	}
	// Deferred by both passes: it is neither annotated nor classified.
	stats, err := w.runRound(context.Background(), roundFast)
	if err != nil || stats.Deferred != 2 || s.lmCalls != 1 {
		t.Fatalf("second fast round: %+v, %v, %d call(s); want it deferred with no model call", stats, err, s.lmCalls)
	}

	clock = clock.Add(firstRetryDelay + time.Second)
	if _, err := w.runRound(context.Background(), roundFast); err != nil || s.lmCalls != 2 {
		t.Fatalf("after the retry delay: %v, %d call(s); want a second attempt", err, s.lmCalls)
	}
	// The second failure doubles the delay: 6 minutes on is not enough.
	clock = clock.Add(firstRetryDelay + time.Second)
	if stats, _ := w.runRound(context.Background(), roundFast); stats.Deferred != 2 || s.lmCalls != 2 {
		t.Fatalf("within the doubled delay: %+v, %d call(s); want deferred", stats, s.lmCalls)
	}

	s.garbled = false
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("complete round: %v", err)
	}
	if !s.annotated[1] || s.queued[1] {
		t.Fatalf("the complete round did not retry the deferred message: annotated=%v queued=%v", s.annotated, s.queued)
	}
}

// TestRoundsWithoutTheQueue: against a epistula-api without migration 022 a fast
// round reports errPassQueueUnavailable before doing any work, and a complete
// round still succeeds, ignoring the prune it cannot make.
func TestRoundsWithoutTheQueue(t *testing.T) {
	s := newFakeStore()
	s.queueDown = true
	w := newClassifyWorker(t, s)

	if _, err := w.runRound(context.Background(), roundFast); !errors.Is(err, errPassQueueUnavailable) {
		t.Fatalf("fast round without the queue = %v; want errPassQueueUnavailable", err)
	}
	if s.lmCalls != 0 {
		t.Fatalf("a fast round without the queue made %d model call(s)", s.lmCalls)
	}
	stats, err := w.RunOnce(context.Background())
	if err != nil || stats.QueueKnown || !s.annotated[1] {
		t.Fatalf("complete round without the queue = %+v, %v; want success with no queue figures", stats, err)
	}
}

// TestRoundSchedule pins serve's choice between the two rounds.
func TestRoundSchedule(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	s := newRoundSchedule(6 * time.Hour)
	if got := s.next(t0); got != roundComplete {
		t.Fatalf("first round = %s; want complete", got)
	}
	s.finished(roundComplete, t0, errors.New("epistula-api down"))
	if got := s.next(t0.Add(time.Minute)); got != roundComplete {
		t.Fatalf("after a failed complete round = %s; want complete again", got)
	}
	s.finished(roundComplete, t0.Add(time.Minute), nil)
	if got := s.next(t0.Add(2 * time.Minute)); got != roundFast {
		t.Fatalf("after a complete round = %s; want fast", got)
	}
	if got := s.next(t0.Add(time.Minute + 6*time.Hour)); got != roundComplete {
		t.Fatalf("six hours on = %s; want complete", got)
	}

	if !s.queueLost() || s.queueLost() {
		t.Fatal("losing the queue should be reported once")
	}
	if got := s.next(t0.Add(3 * time.Minute)); got != roundComplete {
		t.Fatalf("without the queue = %s; want complete", got)
	}
	s.finished(roundComplete, t0.Add(3*time.Minute), nil)
	if got := s.next(t0.Add(4 * time.Minute)); got != roundFast {
		t.Fatalf("after a complete round the queue is tried again; got %s", got)
	}
	s.finished(roundFast, t0.Add(4*time.Minute), nil)
	if !s.queueLost() {
		t.Fatal("after the queue worked again, losing it should be reported again")
	}

	if got := newRoundSchedule(0).next(t0); got != roundComplete {
		t.Fatalf("complete_round_interval_seconds = 0 gives %s; want complete", got)
	}
}
