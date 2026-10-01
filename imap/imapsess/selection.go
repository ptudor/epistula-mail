package imapsess

import (
	"context"
	"errors"
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/imapflags"
)

// readSelection obtains folder counters, the announced UID view, UNSEEN and
// flag vocabulary from one repeatable snapshot. No lock blocks peer delivery.
func (s *Session) readSelection(ctx context.Context, name string) (int64, *imap.SelectData, []viewRow, error) {
	tx, err := s.be.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, nil, err
	}
	defer tx.Rollback(ctx)
	var id, validity, next, modseq int64
	err = tx.QueryRow(ctx, `SELECT id,uidvalidity,uidnext,highest_modseq FROM folders WHERE mailbox_id=$1 AND name=$2`, s.mailboxID, canonicalFolderName(name)).Scan(&id, &validity, &next, &modseq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "no such mailbox"}
	}
	if err != nil {
		return 0, nil, nil, err
	}
	if err := protocolIDs(validity, next); err != nil {
		return 0, nil, nil, err
	}
	if s.selectSnapshotHook != nil {
		s.selectSnapshotHook()
	}
	rows, err := tx.Query(ctx, `SELECT uid,mod_seq,flags FROM messages WHERE folder_id=$1 ORDER BY uid`, id)
	if err != nil {
		return 0, nil, nil, err
	}
	var snapshot []viewRow
	keywords := map[string]struct{}{}
	var unseen uint32
	for rows.Next() {
		var r viewRow
		var flags []string
		if err := rows.Scan(&r.uid, &r.modSeq, &flags); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		if err := protocolIDs(r.uid); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		snapshot = append(snapshot, r)
		if unseen == 0 && !hasFlagFold(flags, `\Seen`) {
			unseen = uint32(len(snapshot))
		}
		for _, flag := range flags {
			if !imapflags.IsSystem(flag) {
				keywords[flag] = struct{}{}
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, nil, err
	}
	names := make([]string, 0, len(keywords))
	for k := range keywords {
		names = append(names, k)
	}
	sort.Strings(names)
	flags := standardFlags()
	for _, k := range names {
		flags = append(flags, imap.Flag(k))
	}
	data := &imap.SelectData{Flags: flags, NumMessages: uint32(len(snapshot)), FirstUnseenSeqNum: unseen, UIDNext: imap.UID(next), UIDValidity: uint32(validity), HighestModSeq: uint64(modseq)}
	return id, data, snapshot, nil
}
