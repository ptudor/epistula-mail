package imapsess

import (
	"context"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// targetUIDs translates only requested ranges through the announced view.
// Sparse ranges use binary search; overlapping ranges are merged before any
// UID slice is copied, preventing duplicate ranges from multiplying allocation.
func (s *Session) targetUIDs(ctx context.Context, set imap.NumSet) ([]int64, error) {
	if err := s.ensureView(ctx); err != nil {
		return nil, err
	}
	if s.view == nil {
		return nil, responseBadState("no mailbox selected")
	}
	if len(s.view.uids) == 0 {
		return nil, nil
	}
	type span struct{ lo, hi int }
	var spans []span
	normal := func(lo, hi, max uint32) (uint32, uint32) {
		if lo == 0 {
			lo = max
		}
		if hi == 0 {
			hi = max
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		return lo, hi
	}
	switch set := set.(type) {
	case imap.SeqSet:
		max := uint32(len(s.view.uids))
		for _, r := range set {
			lo, hi := normal(r.Start, r.Stop, max)
			if lo > max {
				continue
			}
			if hi > max {
				hi = max
			}
			spans = append(spans, span{int(lo) - 1, int(hi)})
		}
	case imap.UIDSet:
		for _, r := range set {
			lo, hi := normal(uint32(r.Start), uint32(r.Stop), uint32(s.view.maxUID()))
			a := sort.Search(len(s.view.uids), func(i int) bool { return s.view.uids[i] >= int64(lo) })
			b := sort.Search(len(s.view.uids), func(i int) bool { return s.view.uids[i] > int64(hi) })
			if a < b {
				spans = append(spans, span{a, b})
			}
		}
	default:
		return nil, responseCannot("unsupported message number set")
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	merged := spans[:0]
	for _, r := range spans {
		if len(merged) > 0 && r.lo <= merged[len(merged)-1].hi {
			if r.hi > merged[len(merged)-1].hi {
				merged[len(merged)-1].hi = r.hi
			}
		} else {
			merged = append(merged, r)
		}
	}
	var out []int64
	for _, r := range merged {
		out = append(out, s.view.uids[r.lo:r.hi]...)
	}
	return out, nil
}

// visitFetchRows releases each bounded result before writing to the client.
// Large JSON metadata is retained for at most eight rows, and blob/MIME bytes
// for one row. It never holds a database connection across a client write.
func (s *Session) visitFetchRows(ctx context.Context, uids []int64, options *imap.FetchOptions, visit func(fetchRow) error) error {
	proj := fetchProjectionFor(options)
	batchSize := 256
	if options != nil && (options.Envelope || options.BodyStructure != nil) {
		batchSize = 8
	}
	for start := 0; start < len(uids); start += batchSize {
		end := start + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		rows, err := s.be.Pool.Query(ctx, `SELECT `+strings.Join(proj.columns, ", ")+` FROM messages WHERE folder_id=$1 AND uid=ANY($2) ORDER BY uid`, s.selectedFolderID, uids[start:end])
		if err != nil {
			return err
		}
		batch := make([]fetchRow, 0, end-start)
		for rows.Next() {
			var r fetchRow
			if err := rows.Scan(proj.targets(&r)...); err != nil {
				rows.Close()
				return err
			}
			r.seqNum = s.view.seqOf(r.uid)
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(batch[i]); err != nil {
				return err
			}
			batch[i] = fetchRow{}
		}
	}
	return nil
}
