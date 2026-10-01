package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// ensureBlobsFunc builds the IngestParams.EnsureBlobs callback for a message
// whose raw and attachment blobs were just written under bucket. It runs
// inside the ingest transaction after the per-blob advisory locks are held,
// re-verifying each file survived any concurrent `gc sweep` and rewriting it
// from the in-memory bytes if not. atts and attParams are parallel slices
// (the same order they were produced in by the MIME walk).
func ensureBlobsFunc(store *blob.Store, tenant blob.Tenant, bucket blob.Bucket, rawSHA string, raw []byte, atts []ingest.Attachment, attParams []storage.AttachmentParams) func(context.Context) error {
	return func(context.Context) error {
		if rewritten, err := store.EnsureContent(blob.KindRaw, tenant, bucket, rawSHA, raw); err != nil {
			return fmt.Errorf("raw blob: %w", err)
		} else if rewritten {
			slog.Warn("raw blob rewritten after concurrent gc sweep", "raw_sha256_short", rawSHA[:16])
		}
		for i := range atts {
			sha := attParams[i].SHA256Hex
			if rewritten, err := store.EnsureContent(blob.KindAttachment, tenant, bucket, sha, atts[i].Data); err != nil {
				return fmt.Errorf("attachment blob %s: %w", attParams[i].PartNumber, err)
			} else if rewritten {
				slog.Warn("attachment blob rewritten after concurrent gc sweep", "sha256_short", sha[:16])
			}
		}
		return nil
	}
}

// openBlobStore initializes the on-disk blob store. deliverPath selects the
// exit-code policy for a permissions/init failure: in the Postfix-facing LDA
// path it must DEFER (EX_TEMPFAIL) so an operator can fix a chmod drift on
// storage_root without Postfix bouncing every inbound message; every other
// subcommand's exit code is read by a human shell, where the semantically
// correct EX_CONFIG for a misconfigured storage_root is the better signal.
func openBlobStore(cfg *Config, deliverPath bool) (*blob.Store, int) {
	return openBlobStoreMode(cfg, deliverPath, true)
}

// openBlobStoreReadOnly opens the store WITHOUT initializing it, for commands
// that advertise themselves as writing nothing (RA6X-053).
//
// Init runs MkdirAll and, in shared-group mode, chmods the root — so a
// "dry-run" pointed at a mistyped storage_root created it, and one pointed at
// a real root could change its permissions. The dry-run flag only ever gated
// message and checkpoint writes; the store setup happened regardless.
//
// A missing or inaccessible root is reported explicitly rather than silently
// created: for a verification pass that is the single most important thing to
// say, because everything the pass reports afterwards is otherwise measured
// against an empty directory.
// openBlobStoreFor picks the opener a maintenance command needs: a dry-run
// gets the read-only form, a live run initializes (RA6X-053).
func openBlobStoreFor(cfg *Config, dryRun bool) (*blob.Store, int) {
	if dryRun {
		return openBlobStoreReadOnly(cfg)
	}
	return openBlobStore(cfg, false)
}

func openBlobStoreReadOnly(cfg *Config) (*blob.Store, int) {
	return openBlobStoreMode(cfg, false, false)
}

func openBlobStoreMode(cfg *Config, deliverPath, initialize bool) (*blob.Store, int) {
	store := blob.NewStore(cfg.Storage.Root)
	store.SetGroupWritable(cfg.Storage.GroupWritable)
	if initialize {
		if err := store.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "blob store: %v\n", err)
			return nil, EX_TEMPFAIL
		}
	} else if fi, err := os.Stat(cfg.Storage.Root); err != nil {
		fmt.Fprintf(os.Stderr,
			"blob store: storage.root %q is not accessible: %v\n"+
				"(this command does not create it — check the path and that the root is mounted)\n",
			cfg.Storage.Root, err)
		return nil, EX_CONFIG
	} else if !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "blob store: storage.root %q is not a directory\n", cfg.Storage.Root)
		return nil, EX_CONFIG
	}
	if cfg.Production {
		// Count non-tenant children so a production start surfaces them too
		// (RO5X-034). Deliver is the hot path and runs once per message, so it
		// stays quiet; the long-lived and maintenance paths report.
		var unknown []string
		if !deliverPath {
			store.SetOnUnknownSubtree(func(name string) { unknown = append(unknown, name) })
			defer store.SetOnUnknownSubtree(nil)
		}
		if err := store.CheckPermissions(); err != nil {
			fmt.Fprintf(os.Stderr, "blob store permissions: %v\n", err)
			if deliverPath {
				return nil, EX_TEMPFAIL
			}
			return nil, EX_CONFIG
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			slog.Warn("unrecognized subtree under storage_root; not permission-checked "+
				"and invisible to gc",
				"names", unknown, "count", len(unknown))
		}
	}
	return store, EX_OK
}
