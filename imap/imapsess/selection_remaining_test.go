package imapsess

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRemainingSelectUsesOneSnapshot(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	s.selectSnapshotHook = func() {
		tx, err := s.be.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE folder_id=$1 AND uid=1`, s.selectedFolderID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE messages SET flags=ARRAY['\Seen','$after'],mod_seq=9 WHERE folder_id=$1 AND uid=2`, s.selectedFolderID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE folders SET uidnext=99,highest_modseq=9 WHERE id=$1`, s.selectedFolderID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.Select("INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.NumMessages != 3 || data.FirstUnseenSeqNum != 1 || data.UIDNext != 4 || !s.view.contains(1) {
		t.Fatalf("mixed SELECT snapshot: %+v / %v", data, s.view.uids)
	}
	for _, f := range data.Flags {
		if f == "$after" {
			t.Fatal("SELECT used later flag vocabulary")
		}
	}
	s.selectSnapshotHook = nil
	data, err = s.Select("INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.NumMessages != 2 || data.FirstUnseenSeqNum != 2 || data.UIDNext != 99 || data.HighestModSeq != 9 || s.view.contains(1) {
		t.Fatalf("fresh SELECT did not see committed changes: %+v", data)
	}
}

func TestRemainingDeletedOrRegeneratedSelectionInvalidates(t *testing.T) {
	for _, deleted := range []bool{true, false} {
		s := mutationFixture(t)
		if _, err := s.Select("INBOX", nil); err != nil {
			t.Fatal(err)
		}
		query := `DELETE FROM folders WHERE id=$1`
		if !deleted {
			query = `UPDATE folders SET uidvalidity=uidvalidity+1 WHERE id=$1`
		}
		if _, err := s.be.Pool.Exec(context.Background(), query, s.selectedFolderID); err != nil {
			t.Fatal(err)
		}
		if err := s.Poll(nil, true); err == nil {
			t.Fatal("stale selected generation remained usable")
		}
		if s.selectedFolderID != 0 || s.sessCtx.Err() == nil {
			t.Fatal("stale selection was not invalidated")
		}
	}
}

func TestRemainingIdlePollsWithExhaustedListenerPool(t *testing.T) {
	f, listenerPool := idlePoolFixture(t, 1)
	f.be.StmtTimeout = 100 * time.Millisecond
	f.be.IdleHeartbeat = 20 * time.Millisecond
	held, err := listenerPool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	srv := imapserver.New(&imapserver.Options{NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		s := f.be.NewSessionForNetConn(c.NetConn())
		s.mailboxID, s.mailboxName, s.tenant = f.mailboxID, f.mailboxName, f.tenant
		return s, &imapserver.GreetingData{PreAuth: true}, nil
	}, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve(ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	read := func() string {
		t.Helper()
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	read()
	fmt.Fprint(c, "a SELECT INBOX\r\n")
	for !strings.HasPrefix(read(), "a OK") {
	}
	fmt.Fprint(c, "b IDLE\r\n")
	if line := read(); !strings.HasPrefix(line, "+") {
		t.Fatal(line)
	}
	tx, err := f.be.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `UPDATE folders SET highest_modseq=highest_modseq+1 WHERE id=$1`, f.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE messages SET flags=ARRAY['$fallback'],mod_seq=mod_seq+1 WHERE folder_id=$1 AND uid=1`, f.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(read(), "$fallback") {
	}
	fmt.Fprint(c, "DONE\r\n")
	for !strings.HasPrefix(read(), "b OK") {
	}
}

type selectedReadCounter struct{ rows atomic.Int64 }

func (c *selectedReadCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (c *selectedReadCounter) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if d.CommandTag.Select() {
		c.rows.Add(d.CommandTag.RowsAffected())
	}
}

func TestRemainingFetchMemoryAndStablePollWork(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	const messages = 2000
	if _, err := s.be.Pool.Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_blob_date,raw_size,internal_date,headers,bodystructure,flags) SELECT $1,n,decode(repeat('ab',32),'hex'),current_date,100,now(),'{}',jsonb_build_object('type','text','subtype','plain','params',jsonb_build_object('padding',repeat('x',32768))),ARRAY[]::text[] FROM generate_series(4,$2) n`, s.selectedFolderID, messages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.be.Pool.Exec(ctx, `UPDATE folders SET uidnext=$2+1 WHERE id=$1`, s.selectedFolderID, messages); err != nil {
		t.Fatal(err)
	}
	trace := &selectedReadCounter{}
	cfg := s.be.Pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s.be.Pool = pool
	if _, err := s.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	trace.rows.Store(0)
	if err := s.Poll(nil, true); err != nil {
		t.Fatal(err)
	}
	if got := trace.rows.Load(); got != 1 {
		t.Fatalf("unchanged Poll transferred %d rows, want one folder row", got)
	}
	trace.rows.Store(0)
	sparse, err := s.targetUIDs(ctx, imap.UIDSetNum(1999))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.visitFetchRows(ctx, sparse, &imap.FetchOptions{UID: true}, func(r fetchRow) error {
		if r.uid != 1999 || r.seqNum != 1999 {
			t.Fatal(r.uid, r.seqNum)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if trace.rows.Load() != 1 {
		t.Fatal("sparse FETCH read unrelated rows")
	}
	uids, err := s.targetUIDs(ctx, imap.UIDSet{{Start: 1, Stop: 0}, {Start: 1, Stop: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != messages {
		t.Fatal("overlapping ranges multiplied target IDs")
	}
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peak uint64
	count := 0
	if err := s.visitFetchRows(ctx, uids, &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}}, func(r fetchRow) error {
		count++
		if count%8 == 0 {
			var current runtime.MemStats
			runtime.ReadMemStats(&current)
			if current.HeapAlloc > peak {
				peak = current.HeapAlloc
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != messages {
		t.Fatal("stream omitted messages", count)
	}
	if peak > baseline.HeapAlloc+24<<20 {
		t.Fatalf("retained FETCH metadata grew by %d bytes", peak-baseline.HeapAlloc)
	}
	t.Logf("2000-row BODYSTRUCTURE read: peak heap growth %d bytes", int64(peak)-int64(baseline.HeapAlloc))
}
