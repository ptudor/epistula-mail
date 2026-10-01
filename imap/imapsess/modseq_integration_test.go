package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// highestModSeq returns the current folders.highest_modseq value.
func highestModSeq(t *testing.T, sess *Session, folderID int64) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT highest_modseq FROM folders WHERE id = $1`, folderID,
	).Scan(&v); err != nil {
		t.Fatalf("highest_modseq lookup: %v", err)
	}
	return v
}

func messageModSeq(t *testing.T, sess *Session, folderID, uid int64) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT mod_seq FROM messages WHERE folder_id = $1 AND uid = $2`, folderID, uid,
	).Scan(&v); err != nil {
		t.Fatalf("mod_seq lookup uid %d: %v", uid, err)
	}
	return v
}

func TestStoreBumpsHighestModSeqAndStampsRow(t *testing.T) {
	sess := mutationFixture(t)
	before := highestModSeq(t, sess, sess.selectedFolderID)

	flags := &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}
	if err := sess.Store(nil, imap.UIDSetNum(1), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}

	after := highestModSeq(t, sess, sess.selectedFolderID)
	if after <= before {
		t.Errorf("highest_modseq = %d, want > %d", after, before)
	}
	if got := messageModSeq(t, sess, sess.selectedFolderID, 1); got != after {
		t.Errorf("uid 1 mod_seq = %d, want %d (stamped with new bump)", got, after)
	}
}

func TestExpungeBumpsHighestModSeq(t *testing.T) {
	sess := mutationFixture(t)
	before := highestModSeq(t, sess, sess.selectedFolderID)
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	after := highestModSeq(t, sess, sess.selectedFolderID)
	if after <= before {
		t.Errorf("EXPUNGE should bump highest_modseq: %d -> %d", before, after)
	}
}

func TestCopyBumpsDestHighestModSeqPerRow(t *testing.T) {
	sess := mutationFixture(t)

	if _, err := sess.Copy(imap.UIDSetNum(1, 2), "Archive/2026"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var destID, destHMS int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id, highest_modseq FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID,
	).Scan(&destID, &destHMS); err != nil {
		t.Fatalf("dest folder lookup: %v", err)
	}
	// Two copies = at least 2 bumps from the auto-created base value (1).
	if destHMS < 3 {
		t.Errorf("dest highest_modseq = %d, want >= 3 after 2 copies", destHMS)
	}
	// Both copied rows must carry a mod_seq stamp (>= 2).
	for _, q := range []string{
		`SELECT mod_seq FROM messages WHERE folder_id = $1 ORDER BY uid LIMIT 1`,
		`SELECT mod_seq FROM messages WHERE folder_id = $1 ORDER BY uid DESC LIMIT 1`,
	} {
		var ms int64
		if err := sess.be.Pool.QueryRow(ctx, q, destID).Scan(&ms); err != nil {
			t.Fatalf("dest mod_seq lookup: %v", err)
		}
		if ms <= 1 {
			t.Errorf("dest row mod_seq = %d, want > 1", ms)
		}
	}
}

func TestSelectReturnsHighestModSeq(t *testing.T) {
	sess := mutationFixture(t)

	// Bump modseq via Store so it's a non-default value.
	flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
	if err := sess.Store(nil, imap.UIDSetNum(1), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	wantHMS := highestModSeq(t, sess, sess.selectedFolderID)

	data, err := sess.Select("INBOX", nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if uint64(wantHMS) != data.HighestModSeq {
		t.Errorf("Select.HighestModSeq = %d, want %d", data.HighestModSeq, wantHMS)
	}
}

func TestSearchModSeqFilters(t *testing.T) {
	sess := mutationFixture(t)

	// Bump modseq on uid 2 only via Store.
	flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
	if err := sess.Store(nil, imap.UIDSetNum(2), flags, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	bumpedHMS := highestModSeq(t, sess, sess.selectedFolderID)

	// SEARCH MODSEQ >= bumpedHMS should return only uid 2.
	criteria := &imap.SearchCriteria{
		ModSeq: &imap.SearchCriteriaModSeq{ModSeq: uint64(bumpedHMS)},
	}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("SEARCH MODSEQ %d = %v, want [2]", bumpedHMS, got)
	}
}
