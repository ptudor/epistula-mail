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
//     messages of the mailboxes the caller may see. Date-ordered keyset
//     batches probe the marker primary key without rebuilding an array of
//     the full queue, and the queue's mailbox_id (migration 029) keeps a
//     scoped reader's cost to its own markers.
//   - POST /v1/pass-required/prune deletes the markers of messages the
//     pipeline has finished: annotated by the caller's model and, when asked,
//     classified into an active category of their mailbox (a mailbox with no
//     categories needs none). A message still missing either keeps its
//     marker, so a failure is retried by the next fast round.
//
// The queue is the fast path. What no insert marks -- a new model, a retired
// category, mail stored before migration 022 -- is the worker's complete
// round, which uses not_annotated_by and not_classified as before.

// passQueueFilter is the pass_required=true predicate: a semi-join from the
// page's messages to the queue, so a date-ordered keyset page probes the
// marker primary key for its own rows and never materializes the queue
// (RM51X-114).
//
// queueScope is an SQL boolean over pr.mailbox_id (epistula-database
// migration 029) that restricts the queue to the mailboxes the query may see:
// the token's scope, or the one mailbox a folder listing is in. It is a
// constant from the call site, never request data. Before the column
// existed the scope was applied only after the queue was joined to messages
// and folders, so a worker scoped to one mailbox paid for the whole queue
// (357K markers for mailboxes nobody annotates: 1.5 s and 1.17M buffers per
// round on a production store) to find that nothing of its own was queued.
// With the scope inside the semi-join the planner can start from the scope's
// own markers through idx_annotation_pass_required_mailbox when they are
// few, and a date-ordered probe never reads a marker outside the scope.
func passQueueFilter(queueScope string) string {
	return `EXISTS (SELECT 1 FROM annotation_pass_required pr WHERE pr.message_id = m.id AND ` + queueScope + `)`
}

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
	if err := checkStorable("mailbox", p.Mailbox); err != nil {
		return err
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
		problemPassQueueUnavailable(w, r, "The annotation pass queue needs epistula-database migration 022 and its grants.")
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
		"($1::bool OR pr.mailbox_id = ANY($2::bigint[]))",
	}
	if req.Mailbox != "" {
		mailboxID, ok := s.lookupScopedMailbox(w, r, tok, req.Mailbox)
		if !ok {
			return
		}
		if !requireMailboxScope(w, r, tok, mailboxID, req.Mailbox) {
			return
		}
		args = append(args, mailboxID)
		conds = append(conds, fmt.Sprintf("pr.mailbox_id = $%d", len(args)))
	}
	// Everything "finished" needs is joined in, set-wise, on keys that admit
	// at most one row each: the annotation by (message_id, model), the
	// classification by message_id, its category by (mailbox_id, key), and
	// whether the mailbox has any active category at all, computed once per
	// mailbox. The previous shape asked the same questions as correlated
	// EXISTS subqueries evaluated per queued row; with 357K markers that was
	// a sequential scan of archive_categories for every one of them, 78M
	// scans reading 4.6 billion rows over a month on a production store. Hash
	// joins read each table once per call.
	//
	// The scope is applied to the queue's own mailbox_id (migration 029), so
	// a scoped caller's prune reads only its markers, and a mailbox nobody
	// annotates never enters the join.
	finished := `an.message_id IS NOT NULL`
	joins := `
			  LEFT JOIN message_annotations an
			         ON an.message_id = pr.message_id AND an.model = $3`
	if req.RequireClassification {
		finished += ` AND (am.mailbox_id IS NULL OR mca.key IS NOT NULL)`
		joins += `
			  LEFT JOIN (SELECT DISTINCT mailbox_id FROM archive_categories WHERE retired_at IS NULL) am
			         ON am.mailbox_id = pr.mailbox_id
			  LEFT JOIN message_classifications mc ON mc.message_id = pr.message_id
			  LEFT JOIN archive_categories mca
			         ON mca.mailbox_id = pr.mailbox_id AND mca.key = mc.category AND mca.retired_at IS NULL`
	}
	scoped := strings.Join(conds, " AND ")
	// One statement: the data-modifying CTE and the count both read the
	// snapshot taken before the DELETE, so remaining is what was queued in
	// scope less what this call cleared.
	query := fmt.Sprintf(`
		WITH queued AS (
			SELECT pr.message_id, (%s) AS finished
			  FROM annotation_pass_required pr%s
			 WHERE %s
		), cleared AS (
			DELETE FROM annotation_pass_required p
			 USING queued q
			 WHERE p.message_id = q.message_id AND q.finished
			RETURNING p.message_id
		)
		SELECT (SELECT count(*) FROM cleared),
		       (SELECT count(*) FROM queued) - (SELECT count(*) FROM cleared)`, finished, joins, scoped)

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
