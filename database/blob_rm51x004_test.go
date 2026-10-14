package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestIngest20MiBVerificationCost(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "benchmark")
	store, params := ingestFixture(t, id, "benchmark")
	raw := bytes.Repeat([]byte("x"), 20<<20)
	bucket := blob.BucketFromTime(params.RawBlobDate)
	sha, _ := writeTestBlob(t, store, "benchmark", bucket, raw)
	params.RawSHA256Hex, params.RawSize = sha, int64(len(raw))
	for _, fast := range []bool{false, true} {
		var inTx time.Duration
		params.EnsureBlobs = func(context.Context) error {
			start := time.Now()
			defer func() { inTx += time.Since(start) }()
			var err error
			if fast {
				_, err = store.EnsureContentFast(blob.KindRaw, "benchmark", bucket, sha, raw)
			} else {
				_, err = store.EnsureContent(blob.KindRaw, "benchmark", bucket, sha, raw)
			}
			return err
		}
		const runs = 20
		start := time.Now()
		for i := 0; i < runs; i++ {
			if _, err := db.Ingest(ctx, params); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("fast=%v: Ingest %s/op; in-transaction blob verification %s/op (20 MiB)", fast, time.Since(start)/runs, inTx/runs)
	}
}
