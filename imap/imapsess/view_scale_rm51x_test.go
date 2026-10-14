package imapsess

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIncrementalReconcileLargeFolder(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	if _, err := s.be.Pool.Exec(ctx, `INSERT INTO messages(folder_id,uid,mod_seq,raw_sha256,raw_size,internal_date)
 SELECT $1,n,1,decode(md5(n::text)||md5(n::text),'hex'),1,now() FROM generate_series(4,100000)n`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.be.Pool.Exec(ctx, `UPDATE folders SET highest_modseq=100 WHERE id=$1`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	s.viewFolderModSeq = 100
	s.viewStampKnown = true
	if _, err := s.be.Pool.Exec(ctx, `UPDATE messages SET mod_seq=101,flags=ARRAY['\Seen'] WHERE folder_id=$1 AND uid=50000`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.be.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rows, delta, incremental, err := s.reconcileSnapshot(ctx, tx)
	elapsed := time.Since(start)
	tx.Rollback(ctx)
	if err != nil || !incremental || len(rows) != 1 || !reflect.DeepEqual(delta.flagChanged, []int64{50000}) || delta.hasExists {
		t.Fatalf("rows=%d delta=%+v incremental=%v err=%v", len(rows), delta, incremental, err)
	}
	t.Logf("100000-message reconciliation reads %d changed row in %s", len(rows), elapsed)
	// A simultaneous arrival and expunge leaves the total unchanged, but must
	// still take the full snapshot so the client receives its EXPUNGE.
	if _, err := s.be.Pool.Exec(ctx, `DELETE FROM messages WHERE folder_id=$1 AND uid=1`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.be.Pool.Exec(ctx, `INSERT INTO messages(folder_id,uid,mod_seq,raw_sha256,raw_size,internal_date) VALUES($1,100001,102,decode(repeat('ab',32),'hex'),1,now())`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	tx, err = s.be.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, delta, incremental, err = s.reconcileSnapshot(ctx, tx)
	if err != nil || incremental || len(rows) != 100000 || !reflect.DeepEqual(delta.expunged, []uint32{1}) {
		t.Fatalf("rows=%d delta=%+v incremental=%v err=%v", len(rows), delta, incremental, err)
	}
}

func TestRemoveManyRenumbering(t *testing.T) {
	v := newSessionView([]viewRow{{1, 1}, {3, 1}, {5, 1}, {7, 1}, {9, 1}})
	if got := v.removeMany([]int64{0, 3, 4, 7, 9, 10}); !reflect.DeepEqual(got, []uint32{2, 3, 3}) {
		t.Fatalf("expunges=%v", got)
	}
	if !reflect.DeepEqual(v.uids, []int64{1, 5}) || len(v.modSeq) != 2 {
		t.Fatalf("view=%+v", v)
	}
}

func BenchmarkRemoveManyHalfOf200K(b *testing.B) {
	rows := make([]viewRow, 200000)
	uids := make([]int64, 100000)
	for i := range rows {
		rows[i] = viewRow{int64(i + 1), 1}
	}
	for i := range uids {
		uids[i] = int64(2*i + 1)
	}
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		v := newSessionView(rows)
		b.StartTimer()
		seqs := v.removeMany(uids)
		if len(seqs) != 100000 || v.len() != 100000 {
			b.Fatal("incorrect removal")
		}
	}
}
