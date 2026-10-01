package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestFetchTargetsSeqNumIsFolderWide is the R-040 regression guard: the two-step
// resolve→detail path must carry the FOLDER-WIDE sequence number, not the
// matched subset's position, and must still load the heavy JSONB for the
// matched row. Fetching only uid 3 (the folder's last message) must report
// seqNum 3, not 1.
func TestFetchTargetsSeqNumIsFolderWide(t *testing.T) {
	sess := searchFixture(t) // 3 messages, uid 1/2/3 == seqNum 1/2/3
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// BODYSTRUCTURE is requested here, because the heavy JSONB this test asserts on
	// is now loaded only when a requested item reads it (RA6X-054). The R-040
	// property under test — that the detail query loads the heavy columns for
	// the MATCHED rows rather than for the whole folder — is unchanged.
	rows, err := sess.fetchTargets(ctx, imap.UIDSet{{Start: 3, Stop: 3}},
		&imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 matched row, got %d", len(rows))
	}
	if rows[0].uid != 3 || rows[0].seqNum != 3 {
		t.Fatalf("uid=%d seqNum=%d, want uid=3 seqNum=3 (folder-wide position)", rows[0].uid, rows[0].seqNum)
	}
	if len(rows[0].bodyStructure) == 0 {
		t.Error("detail fetch did not load the BODYSTRUCTURE JSONB for the matched row")
	}
	// And a FETCH that asks for none of it loads none of it.
	lean, err := sess.fetchTargets(ctx, imap.UIDSet{{Start: 3, Stop: 3}},
		&imap.FetchOptions{UID: true, Flags: true})
	if err != nil {
		t.Fatalf("fetchTargets (lean): %v", err)
	}
	if len(lean) != 1 || lean[0].uid != 3 || lean[0].seqNum != 3 {
		t.Fatalf("lean fetch = %+v, want the same identity", lean)
	}
	if len(lean[0].headers) != 0 || len(lean[0].bodyStructure) != 0 {
		t.Error("a UID/FLAGS fetch still loaded the headers or bodystructure JSONB")
	}

	// The lightweight resolver returns (seqNum, uid, rawSize) with the same
	// folder-wide seq mapping — message 2 has size 1024 in the fixture.
	refs, err := sess.resolveTargets(ctx, imap.UIDSet{{Start: 2, Stop: 2}})
	if err != nil {
		t.Fatalf("resolveTargets: %v", err)
	}
	if len(refs) != 1 || refs[0].uid != 2 || refs[0].seqNum != 2 || refs[0].rawSize != 1024 {
		t.Fatalf("resolveTargets ref = %+v, want {seqNum:2 uid:2 rawSize:1024}", refs[0])
	}
}
