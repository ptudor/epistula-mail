package main

import "time"

// roundSchedule decides whether serve's next round is fast or complete.
//
// The first round after start is complete, because whatever happened while
// the worker was down was queued for nobody in particular and may include
// work no insert queues. After that a round is complete once completeEvery
// has passed since the last complete round that succeeded; a failed one is
// retried at the next round rather than an interval later. completeEvery <= 0
// makes every round complete.
//
// A epistula-api without the queue turns fast rounds off until the next complete
// round succeeds, after which the queue is tried again, so upgrading epistula-api
// under a running worker needs no restart.
type roundSchedule struct {
	completeEvery time.Duration
	lastComplete  time.Time
	queue         bool
	// warned is whether the loss of the queue has been logged since it last
	// worked, so a epistula-api that stays without it is reported once.
	warned bool
}

func newRoundSchedule(completeEvery time.Duration) *roundSchedule {
	return &roundSchedule{completeEvery: completeEvery, queue: true}
}

func (s *roundSchedule) next(now time.Time) roundKind {
	if !s.queue || s.completeEvery <= 0 || s.lastComplete.IsZero() || now.Sub(s.lastComplete) >= s.completeEvery {
		return roundComplete
	}
	return roundFast
}

// queueLost records that a fast round found no queue, and reports whether
// that is news worth logging.
func (s *roundSchedule) queueLost() bool {
	s.queue = false
	news := !s.warned
	s.warned = true
	return news
}

// finished records a round's outcome.
func (s *roundSchedule) finished(round roundKind, started time.Time, err error) {
	switch {
	case round == roundComplete && err == nil:
		s.lastComplete = started
		s.queue = true
	case round == roundFast && err == nil:
		s.warned = false
	}
}
