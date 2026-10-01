package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ptudor/epistula-mail/database/auth"
)

// The annotation pipeline's work queue (epistula-database migration 022).
//
// epistula-database marks every message in annotation_pass_required in the
// transaction that stores it, and epistula-imap's COPY and MOVE carry the
// marker. epistula-api serves the queue two ways:
//
//   - pass_required=true on list, search and export selects the marked
//     messages. The ids are read out of the queue first and the messages
//     fetched by primary key, so the cost follows the queue, not the store. Asking
//     "which messages have no annotation" instead probes every message.
//   - POST /v1/pass-required/prune deletes the markers of messages the
//     pipeline has finished: annotated by the caller's model and, when asked,
//     classified into an active category of their mailbox (a mailbox with no
//     categories needs none). A message still missing either keeps its
//     marker, so a failure is retried by the next fast round.
//
// The queue is the fast path. What no insert marks -- a new model, a retired
// category, mail stored before migration 022 -- is the worker's complete
// round, which uses not_annotated_by and not_classified as before.

// passQueueFilter is the pass_required=true condition. ARRAY(SELECT ...) is
// evaluated once, and m.id = ANY(...) then drives an index scan on the
// messages primary key whatever the queue's size or statistics. The obvious
// "m.id IN (SELECT ...)" let the planner merge-join in id order instead, and
// since the queue holds the newest ids it walked every older message first.
const passQueueFilter = `m.id = ANY(ARRAY(SELECT pr.message_id FROM annotation_pass_required pr))`

// maxPassPruneBytes bounds the prune request body; it carries a model name, a
// mailbox name and a flag.
const maxPassPruneBytes = 4096

type passPruneRequest struct {
	Model                 string `json:"model"`
	Mailbox               string `json:"mailbox,omitempty"`
	RequireClassification bool   `json:"require_classification"`
}

type passPruneResult struct {
	Pruned    int64 `json:"pruned"`
	Remaining int64 `json:"remaining"`
}

func (p *passPruneRequest) validate() error {
	p.Model = strings.TrimSpace(p.Model)
	p.Mailbox = strings.TrimSpace(p.Mailbox)
	if p.Model == "" {
		return errors.New("model is required")
	}
	if len(p.Model) > maxAnnotationModelLen {
		return fmt.Errorf("model exceeds %d bytes", maxAnnotationModelLen)
	}
	return checkStorable("model", p.Model)
}

// handlePassRequiredPrune clears the markers of finished messages in the
// token's scope (or one mailbox of it) and reports how many were cleared and
// how many remain. Clearing is set-based and driven by the queue, so it costs
// what the queue holds. It needs write_annotation: it is the pipeline saying
// its own work is done, and it can only drop a message from the fast round,
// never from the complete one.
func (s *server) handlePassRequiredPrune(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionWriteAnnotation) {
		return
	}
	if !s.passQueueAvailable {
		problemUnavailable(w, r, "The annotation pass queue needs epistula-database migration 022 and its grants.")
		return
	}
	var req passPruneRequest
	if !decodeJSONBody(w, r, &req, maxPassPruneBytes) {
		return
	}
	if err := req.validate(); err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}
	if req.RequireClassification && !s.classificationsAvailable {
		problemUnavailable(w, r, "Classification filters require epistula-database migration 020 and its grants.")
		return
	}

	all, scope := scopeParams(tok)
	args := []any{all, scope, req.Model}
	conds := []string{
		"($1::bool OR f.mailbox_id = ANY($2::bigint[]))",
	}
	if req.Mailbox != "" {
		mailboxID, ok := s.lookupMailbox(w, r, req.Mailbox)
		if !ok {
			return
		}
		if !requireMailboxScope(w, r, tok, mailboxID, req.Mailbox) {
			return
		}
		args = append(args, mailboxID)
		conds = append(conds, fmt.Sprintf("f.mailbox_id = $%d", len(args)))
	}
	done := `EXISTS (SELECT 1 FROM message_annotations an WHERE an.message_id = m.id AND an.model = $3)`
	if req.RequireClassification {
		done += `
		   AND (NOT EXISTS (SELECT 1 FROM archive_categories ac
		                     WHERE ac.mailbox_id = f.mailbox_id AND ac.retired_at IS NULL)
		        OR EXISTS (SELECT 1 FROM message_classifications mc
		                     JOIN archive_categories mca
		                       ON mca.key = mc.category AND mca.retired_at IS NULL
		                    WHERE mc.message_id = m.id AND mca.mailbox_id = f.mailbox_id))`
	}
	scoped := strings.Join(conds, " AND ")
	// One statement: the data-modifying CTE and the count both read the
	// snapshot taken before the DELETE, so remaining is what was queued in
	// scope less what this call cleared.
	query := fmt.Sprintf(`
		WITH queued AS (
			SELECT pr.message_id, %s AS finished
			  FROM annotation_pass_required pr
			  JOIN messages m ON m.id = pr.message_id
			  JOIN folders f ON f.id = m.folder_id
			 WHERE %s
		), cleared AS (
			DELETE FROM annotation_pass_required p
			 USING queued q
			 WHERE p.message_id = q.message_id AND q.finished
			RETURNING p.message_id
		)
		SELECT (SELECT count(*) FROM cleared),
		       (SELECT count(*) FROM queued) - (SELECT count(*) FROM cleared)`, done, scoped)

	var res passPruneResult
	if err := s.pool.QueryRow(r.Context(), query, args...).Scan(&res.Pruned, &res.Remaining); err != nil {
		slog.Error("pass queue prune", "model", req.Model, "err", err)
		problemInternal(w, r)
		return
	}
	metricPassMarkersPruned.Add(float64(res.Pruned))
	slog.Info("pass queue pruned", "model", req.Model, "mailbox", req.Mailbox, "token", tok.Name,
		"pruned", res.Pruned, "remaining", res.Remaining)
	writeJSON(w, http.StatusOK, res)
}
