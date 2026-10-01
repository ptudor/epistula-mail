package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Archive sorting, the classifier's side (ARCHIVE_SORTING.md at the repository
// root; epistula-database migration 020).
//
// epistula-api still moves nothing. It serves a mailbox's approved category list
// and stores the classifier's choice from it; epistula-imap's sorter is what
// files a message, and only once the user has archived it. The category is
// checked here against the mailbox's ACTIVE list, so a classification can
// only ever name a folder the operator approved — whatever the message that
// was classified said about itself.

type archiveCategoryItem struct {
	Key         string `json:"key"`
	Folder      string `json:"folder"`
	Description string `json:"description,omitempty"`
	// Annual categories file into <folder>/<YYYY>, the year from the
	// message's own date.
	Annual bool `json:"annual,omitempty"`
}

// ---- GET /v1/mailboxes/{mailbox}/archive-categories ----

// handleArchiveCategories returns the mailbox's active categories and its
// \Archive folder. An empty list means archive sorting is not set up for the
// mailbox, which a classifier treats as "nothing to classify here".
func (s *server) handleArchiveCategories(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionReadMetadata) {
		return
	}
	if !s.classificationsAvailable {
		problemUnavailable(w, r, "Archive categories require epistula-database migration 020 and its grants.")
		return
	}
	mailbox := r.PathValue("mailbox")
	mailboxID, ok := s.lookupMailbox(w, r, mailbox)
	if !ok {
		return
	}
	if !requireMailboxScope(w, r, tok, mailboxID, mailbox) {
		return
	}
	var archiveFolder *string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT name FROM folders WHERE mailbox_id = $1 AND special_use = '\Archive' ORDER BY id LIMIT 1`,
		mailboxID,
	).Scan(&archiveFolder); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("archive folder lookup", "err", err)
		problemInternal(w, r)
		return
	}
	rows, err := s.pool.Query(r.Context(),
		`SELECT key, folder, description, annual FROM archive_categories
		  WHERE mailbox_id = $1 AND retired_at IS NULL ORDER BY key`, mailboxID)
	if err != nil {
		slog.Error("archive categories query", "err", err)
		problemInternal(w, r)
		return
	}
	defer rows.Close()
	out := []archiveCategoryItem{}
	for rows.Next() {
		var c archiveCategoryItem
		if err := rows.Scan(&c.Key, &c.Folder, &c.Description, &c.Annual); err != nil {
			slog.Error("archive categories scan", "err", err)
			problemInternal(w, r)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		slog.Error("archive categories rows", "err", err)
		problemInternal(w, r)
		return
	}
	resp := map[string]any{"mailbox": mailbox, "categories": out}
	if archiveFolder != nil {
		resp["archive_folder"] = *archiveFolder
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- PUT /v1/messages/{id}/classification ----

type classificationPut struct {
	Category   string   `json:"category"`
	Confidence *float64 `json:"confidence"`
	Model      string   `json:"model"`
}

// maxClassificationBody bounds the PUT body; the document is three short
// fields.
const maxClassificationBody = 4096

func (p *classificationPut) validate() error {
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
	if !storage.ValidArchiveKey(p.Category) {
		return errors.New("category must be a category key: one to three lower-case [a-z0-9-] segments separated by \"/\"")
	}
	if p.Confidence == nil {
		return errors.New("confidence is required")
	}
	if c := *p.Confidence; math.IsNaN(c) || c < 0 || c > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	return nil
}

func (s *server) handleClassificationPut(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionWriteClassification) {
		return
	}
	if !s.classificationsAvailable {
		problemUnavailable(w, r, "Classifications require epistula-database migration 020 and its grants.")
		return
	}
	id, ok := parseMessageID(w, r)
	if !ok {
		return
	}
	var put classificationPut
	if !decodeJSONBody(w, r, &put, maxClassificationBody) {
		return
	}
	if err := put.validate(); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}
	ref := s.resolveMessage(w, r, id)
	if ref == nil {
		return
	}
	if !requireMailboxScope(w, r, tok, ref.MailboxID, ref.Mailbox) {
		return
	}

	err := s.upsertClassification(r.Context(), id, ref.MailboxID, put)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errInactiveCategory):
		problemUnprocessable(w, r, fmt.Sprintf("category %q is not an active archive category of mailbox %q", put.Category, ref.Mailbox))
		return
	case errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgErr) && pgErr.Code == "23503"):
		// Expunged between the resolve and the write: the same "gone, skip"
		// answer the annotation PUT gives (R-065).
		problemNotFound(w, r, "No such message.")
		return
	case err != nil:
		slog.Error("classification upsert", "message_id", id, "err", err)
		problemInternal(w, r)
		return
	}
	metricClassificationsWritten.Inc()
	slog.Info("classification stored",
		"message_id", id, "model", put.Model, "token", tok.Name,
		"category", put.Category, "confidence", *put.Confidence)
	w.WriteHeader(http.StatusNoContent)
}

var errInactiveCategory = errors.New("category is not active for the mailbox")

// upsertClassification stores one message's classification, checking the
// category against the mailbox's active list inside the same transaction.
//
// The parent message is locked first, exactly as an annotation write does
// (mail_lock_message_for_annotation, migration 019), so a competing IMAP MOVE
// either copies this committed classification or waits and removes the
// message; the classification never lands on a row MOVE has already copied
// past.
func (s *server) upsertClassification(ctx context.Context, id, mailboxID int64, put classificationPut) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT locked FROM mail_lock_message_for_annotation($1) AS locked`, id).Scan(&lockedID); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM archive_categories
		                 WHERE mailbox_id = $1 AND key = $2 AND retired_at IS NULL)`,
		mailboxID, put.Category,
	).Scan(&active); err != nil {
		return err
	}
	if !active {
		return errInactiveCategory
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO message_classifications (message_id, category, confidence, model)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (message_id) DO UPDATE
		   SET category = EXCLUDED.category, confidence = EXCLUDED.confidence,
		       model = EXCLUDED.model, created_at = now()`,
		id, put.Category, *put.Confidence, put.Model,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- filters ----

// addArchiveFilters appends the archive-sorting filters shared by folder
// listing, search and export:
//
//   - not_classified=true selects messages with no classification into an
//     active category of their mailbox: a classifier's undone work, including
//     messages classified against a category since retired. Like
//     not_annotated_by, it answers "has the pipeline run over this message",
//     not "what does it say", so a metadata-only token may use it.
//   - pass_required=true selects the messages queued for the annotation
//     pipeline (passrequired.go), read through the queue rather than by
//     probing every message. Operational state, so a metadata-only token may
//     use it too.
//   - sample_ppm=N selects a deterministic pseudo-random N-in-a-million of
//     messages, keyed on the message id, so a sampled pass is repeatable and a
//     resumed one sees the same sample. Taxonomy discovery uses it to read a
//     few percent of a large archive instead of all of it.
//
// mailboxCol is the SQL expression, in the caller's query, for the message's
// mailbox id: f.mailbox_id where the query joins folders as f, or an
// uncorrelated subquery where the folder is fixed. It is a constant from the
// call site, never request data.
//
// Writes the error response and returns false on a bad value.
func (s *server) addArchiveFilters(w http.ResponseWriter, r *http.Request, mailboxCol string, conds *[]string, args *[]any) bool {
	q := r.URL.Query()
	if v := q.Get("not_classified"); v != "" {
		want, err := strconv.ParseBool(v)
		if err != nil {
			problemUnprocessable(w, r, "not_classified must be true or false")
			return false
		}
		if want {
			if !s.classificationsAvailable {
				problemUnavailable(w, r, "Classification filters require epistula-database migration 020 and its grants.")
				return false
			}
			// Correlated only through WHERE, so the planner turns it into an
			// anti-join. It used to join its own folders row ON
			// mcf.id = m.folder_id; a join qual on the outer row kept it a
			// per-row SubPlan, and once nearly every message was classified
			// the worker's scan for the last few took 17 s on a 267K-message
			// archive, past the 10 s statement_timeout, on every pass.
			*conds = append(*conds, fmt.Sprintf(`NOT EXISTS (
				SELECT 1 FROM message_classifications mc
				  JOIN archive_categories mca
				    ON mca.key = mc.category AND mca.retired_at IS NULL
				 WHERE mc.message_id = m.id AND mca.mailbox_id = %s)`, mailboxCol))
		}
	}
	if v := q.Get("pass_required"); v != "" {
		want, err := strconv.ParseBool(v)
		if err != nil {
			problemUnprocessable(w, r, "pass_required must be true or false")
			return false
		}
		if want {
			if !s.passQueueAvailable {
				problemUnavailable(w, r, "The annotation pass queue needs epistula-database migration 022 and its grants.")
				return false
			}
			*conds = append(*conds, passQueueFilter)
		}
	}
	if v := q.Get("sample_ppm"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1_000_000 {
			problemUnprocessable(w, r, "sample_ppm must be an integer from 1 to 1000000")
			return false
		}
		if n < 1_000_000 {
			// hashint8extended is the hash PostgreSQL's hash partitioning
			// relies on, so it is stable across upgrades; the remainder of a
			// signed value is folded with abs so the sample is uniform.
			*args = append(*args, n)
			*conds = append(*conds, fmt.Sprintf("abs(hashint8extended(m.id, 0) %% 1000000) < $%d", len(*args)))
		}
	}
	return true
}
