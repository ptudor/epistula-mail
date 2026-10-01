package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ptudor/epistula-mail/database/auth"
)

// PUT /v1/messages/{id}/annotation — the daemon's single write path.
// Stores/replaces the derived sidecar (tags, advisory category, optional
// summary) keyed (message_id, model). Never touches messages, folders,
// blobs, or UIDs.

type annotationPut struct {
	Model     string   `json:"model"`
	Tags      []string `json:"tags"`
	Category  *string  `json:"category"`
	Summary   *string  `json:"summary"`
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
}

const (
	maxAnnotationModelLen    = 128
	maxAnnotationTagCount    = 64
	maxAnnotationTagLen      = 128
	maxAnnotationCategoryLen = 256
)

// checkStorable rejects a string PostgreSQL cannot hold in a text column
// (RA6X-041).
//
// A JSON string containing U+0000 is syntactically valid and passed every
// length and emptiness check here, then failed at the INSERT with
// `unsupported Unicode escape sequence` — surfaced to the caller as a 500,
// which says "the server is broken" about a request the server should have
// refused. NUL is the only byte value text rejects; Go's JSON decoder already
// guarantees the rest is well-formed UTF-8 (invalid bytes become U+FFFD), so
// this one check is the whole storage contract.
func checkStorable(field, v string) error {
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("%s contains a NUL character, which cannot be stored", field)
	}
	return nil
}

func (p *annotationPut) validate() error {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		return errors.New("model is required")
	}
	if len(p.Model) > maxAnnotationModelLen {
		return fmt.Errorf("model exceeds %d bytes", maxAnnotationModelLen)
	}
	if err := checkStorable("model", p.Model); err != nil {
		return err
	}
	if len(p.Tags) > maxAnnotationTagCount {
		return fmt.Errorf("tags exceed %d entries", maxAnnotationTagCount)
	}
	for i, tag := range p.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return errors.New("tags must not contain empty strings")
		}
		if len(tag) > maxAnnotationTagLen {
			return fmt.Errorf("tag exceeds %d bytes", maxAnnotationTagLen)
		}
		if err := checkStorable(fmt.Sprintf("tag %d", i+1), tag); err != nil {
			return err
		}
		p.Tags[i] = tag
	}
	if p.Category != nil {
		if len(*p.Category) > maxAnnotationCategoryLen {
			return fmt.Errorf("category exceeds %d bytes", maxAnnotationCategoryLen)
		}
		if err := checkStorable("category", *p.Category); err != nil {
			return err
		}
	}
	if p.Summary != nil {
		if err := checkStorable("summary", *p.Summary); err != nil {
			return err
		}
	}
	if p.TokensIn != nil && *p.TokensIn < 0 {
		return errors.New("tokens_in must be non-negative")
	}
	if p.TokensOut != nil && *p.TokensOut < 0 {
		return errors.New("tokens_out must be non-negative")
	}
	return nil
}

func (s *server) handleAnnotationPut(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionWriteAnnotation) {
		return
	}
	if !s.annotationsAvailable {
		problemUnavailable(w, r, "The message_annotations migration has not been applied in epistula-database yet.")
		return
	}
	id, ok := parseMessageID(w, r)
	if !ok {
		return
	}

	var put annotationPut
	if !decodeJSONBody(w, r, &put, s.cfg.Limits.MaxAnnotationBytes) {
		return
	}
	if err := put.validate(); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}
	if put.Tags == nil {
		put.Tags = []string{}
	}

	ref := s.resolveMessage(w, r, id)
	if ref == nil {
		return
	}
	if !requireMailboxScope(w, r, tok, ref.MailboxID, ref.Mailbox) {
		return
	}

	// Idempotent on (message_id, model): re-running a model replaces its
	// annotation; created_at reflects the latest run.
	if err := s.upsertAnnotation(r.Context(), id, put); err != nil {
		// resolveMessage succeeded, but epistula-imap can expunge the message
		// between the resolve and this upsert; the FK on
		// message_annotations.message_id then fails as SQLSTATE 23503. That's
		// "message gone, skip", not a server bug — return the same 404 as an
		// absent message so the worker treats it as done rather than a hard
		// pass failure (R-065).
		var pgErr *pgconn.PgError
		if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgErr) && pgErr.Code == "23503") {
			problemNotFound(w, r, "No such message.")
			return
		}
		slog.Error("annotation upsert", "message_id", id, "model", put.Model, "err", err)
		problemInternal(w, r)
		return
	}
	metricAnnotationsWritten.Inc()
	slog.Info("annotation stored",
		"message_id", id, "model", put.Model, "token", tok.Name,
		"tags", len(put.Tags), "has_summary", put.Summary != nil)
	w.WriteHeader(http.StatusNoContent)
}

// Lock the parent before touching a sidecar, including ON CONFLICT updates.
// PostgreSQL does not recheck an unchanged FK during DO UPDATE. Without this
// lock a PUT could succeed after MOVE copied the old annotation, then be lost
// in MOVE's cascade. Competing MOVE either copies this committed PUT or the
// PUT waits and reports 404 after MOVE has removed the original message.
//
// The lock is taken by mail_lock_message_for_annotation (migration 019), not
// by a FOR KEY SHARE here: a row lock needs UPDATE privilege on messages, and
// this role has SELECT only, so the direct form failed every write with
// "permission denied for table messages" (OPS-008). The function holds the
// same lock for this transaction and returns no row for a missing message.
func (s *server) upsertAnnotation(ctx context.Context, id int64, put annotationPut) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT locked FROM mail_lock_message_for_annotation($1) AS locked`, id).Scan(&lockedID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO message_annotations (message_id, model, tags, category, summary, tokens_in, tokens_out)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (message_id, model) DO UPDATE
		   SET tags = EXCLUDED.tags, category = EXCLUDED.category,
		       summary = EXCLUDED.summary, tokens_in = EXCLUDED.tokens_in,
		       tokens_out = EXCLUDED.tokens_out, created_at = now()`,
		id, put.Model, put.Tags, put.Category, put.Summary, put.TokensIn, put.TokensOut,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
