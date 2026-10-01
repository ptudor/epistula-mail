package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func repairDuplicateDelivery(ctx context.Context, db *storage.DB, cfg *Config, mailboxID int64, mailboxName string, msg *ingest.Message, raw []byte, accepted *deliveryAcceptance) (bool, error) {
	tenant, err := blob.ParseTenant(mailboxName)
	if err != nil {
		return false, err
	}
	store, code := openBlobStore(cfg, true)
	if code != EX_OK {
		return false, fmt.Errorf("open blob store: %s", ExitCodeName(code))
	}
	return db.RepairDuplicate(ctx, mailboxID, mailboxName, cfg.Delivery.DefaultFolder, msg.SHA256Hex, duplicateContentRepair(ctx, store, tenant, msg, raw), accepted.markCommitted)
}

func duplicateContentRepair(ctx context.Context, store *blob.Store, tenant blob.Tenant, msg *ingest.Message, raw []byte) func([]storage.StoredBlob) error {
	// Match old references by content, not by current MIME part numbering:
	// historical parser versions may have numbered the same attachment differently.
	content := make(map[string][]byte, len(msg.Attachments))
	for _, att := range msg.Attachments {
		sum := sha256.Sum256(att.Data)
		content[hex.EncodeToString(sum[:])] = att.Data
	}
	return func(refs []storage.StoredBlob) error {
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			ok, err := store.ContentMatches(ref.Kind, tenant, ref.Bucket, ref.SHA, ref.Size)
			if err != nil {
				return err
			}
			if ok {
				continue
			}
			data, available := content[ref.SHA]
			if ref.Kind == blob.KindRaw {
				data, available = raw, true
			}
			if !available || int64(len(data)) != ref.Size {
				return fmt.Errorf("cannot reconstruct stored %s blob of size %d", ref.Kind, ref.Size)
			}
			if _, err := store.EnsureContent(ref.Kind, tenant, ref.Bucket, ref.SHA, data); err != nil {
				return err
			}
		}
		return nil
	}
}
