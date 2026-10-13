package imapsess

import (
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// TestFolderSweepIsPacedOverTheWire is the production report end to end. A
// mail client checking a mailbox with thousands of folders pipelines one
// STATUS per folder every few minutes; every command past the first second of
// the command-rate bucket used to come back NO [LIMIT], so those folders were
// never checked. Through the real server and a real client, a pipelined sweep
// of twice the bucket must now be answered in full, at the configured rate.
func TestFolderSweepIsPacedOverTheWire(t *testing.T) {
	sess, _ := appendFixture(t)
	const folders = 8
	for i := 0; i < folders; i++ {
		mustCreateFolder(t, sess, fmt.Sprintf("Archive/%02d", i))
	}
	const rate = 20
	sess.be.CommandRatePerSec = rate // sessions the wire client opens use it
	client := chainWireClient(t, sess)

	// Pipeline the whole sweep before reading any response, as a client does.
	const commands = 2 * rate
	start := time.Now()
	pending := make([]*imapclient.StatusCommand, 0, commands)
	for i := 0; i < commands; i++ {
		pending = append(pending, client.Status(fmt.Sprintf("Archive/%02d", i%folders),
			&imap.StatusOptions{NumMessages: true, UIDNext: true}))
	}
	for i, cmd := range pending {
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("STATUS %d of %d failed: %v", i+1, commands, err)
		}
	}
	// The burst is served at once and the rest at the rate: about one second
	// for the second half. Allow scheduling slack below, not above.
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
		t.Errorf("%d pipelined commands at %d/s finished in %v; the second half should be paced over about 1s",
			commands, rate, elapsed)
	}
}
