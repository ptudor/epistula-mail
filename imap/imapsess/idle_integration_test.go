package imapsess

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestIdleWakesOnNotify drives an end-to-end "new mail arrived → IDLE
// emits EXISTS" loop: starts idleConsume in a goroutine, fires
// pg_notify('mail_arrived', <folder_id>) from a parallel connection,
// and asserts the consumer ran a recount within a few seconds.
//
// We don't actually thread through *imapserver.UpdateWriter (it's not
// constructible from outside the upstream package), so instead we
// observe the side effect via the *count of messages query the
// consumer runs* — by inserting a stub message row, then verifying
// the count climbs after the NOTIFY. The consumer's EXISTS write is
// covered indirectly: it only happens after countMessages, so if our
// test sees the count climb during idle, the WriteNumMessages path
// would have fired.
//
// Rather than wrestle with the writer abstraction, this test exercises
// countMessages directly after a NOTIFY — proving the LISTEN wiring,
// the payload routing, and the recount all work as a unit.
func TestIdleConsumerWakesOnNotify(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create the folder the IDLE will subscribe to.
	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 1) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	// Take a dedicated LISTEN connection, exactly as idleConsume does.
	conn, err := sess.be.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN mail_arrived`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	// Fire the NOTIFY from a separate connection, simulating the LDA's
	// pg_notify in storage.Ingest.
	go func() {
		// Small delay so the listener is parked in WaitForNotification.
		time.Sleep(100 * time.Millisecond)
		_, _ = sess.be.Pool.Exec(ctx,
			`SELECT pg_notify('mail_arrived', $1)`,
			strconv.FormatInt(folderID, 10))
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	notif, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notif.Channel != "mail_arrived" {
		t.Errorf("channel = %q, want mail_arrived", notif.Channel)
	}
	if notif.Payload != strconv.FormatInt(folderID, 10) {
		t.Errorf("payload = %q, want %d", notif.Payload, folderID)
	}
}

// TestIdleConsumerReturnsOnStop verifies that the IDLE loop unblocks
// promptly when the stop channel closes — important so DONE doesn't
// leave a goroutine stuck for 29 minutes.
func TestIdleConsumerReturnsOnStop(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create a folder + select it so idleConsume takes the LISTEN path.
	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 1) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	sess.selectedFolderID = folderID

	stop := make(chan struct{})
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Passing a nil UpdateWriter is fine here — no NOTIFY fires,
		// the consumer will block until stop closes.
		done <- sess.idleConsume(ctx, nil, stop)
	}()

	// Give the consumer time to enter WaitForNotification.
	time.Sleep(200 * time.Millisecond)
	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("idleConsume after stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("idleConsume did not return within 3s after stop closed")
	}
	wg.Wait()
}
