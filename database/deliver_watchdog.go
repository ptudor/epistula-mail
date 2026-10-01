package main

import (
	"sync"
	"time"
)

// A stopped timer is joined even when its callback was already running.
// Repeated stops are safe: a canceled callback will never close done itself.
type deliveryTimer struct {
	timer    *time.Timer
	done     chan struct{}
	stopOnce sync.Once
}

func newDeliveryTimer(d time.Duration, fn func()) *deliveryTimer {
	t := &deliveryTimer{done: make(chan struct{})}
	t.timer = time.AfterFunc(d, func() { defer close(t.done); fn() })
	return t
}
func (t *deliveryTimer) stop() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() {
		if t.timer.Stop() {
			close(t.done)
		}
	})
	<-t.done
}

// Commit replaces the delivery deadline with a separate success-only budget.
// A callback that already started is joined before any hook can be launched.
// The hook's own timeout kills its process group; this final one-second grace
// also bounds terminal logging or a stuck Wait after that cancellation.
func armDeliveryWatchdogs(a *deliveryAcceptance, delivery, postCommit time.Duration, recipient string) func() {
	primary := newDeliveryTimer(delivery, func() { deliveryDeadlineExceeded(a, delivery, recipient) })
	var post *deliveryTimer
	a.afterCommit = func() {
		primary.stop()
		post = newDeliveryTimer(postCommit, func() { deliveryDeadlineExceeded(a, postCommit, recipient) })
	}
	// afterCommit runs synchronously on the delivering goroutine, before it
	// returns to this cleanup; post is never read/written concurrently.
	return func() { primary.stop(); post.stop() }
}
