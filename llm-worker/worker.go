package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"
)

// consecutiveInfraFailureAbort is how many back-to-back infrastructure
// failures (LM Studio unreachable, epistula-api unreachable, or a revoked token)
// end the pass early. Such failures affect every message, so churning the
// whole corpus — re-running LLM inference and streaming bodies for each — is
// futile. Per-message content errors (decode, truncation) do not count and
// reset the run. See R-037.
const consecutiveInfraFailureAbort = 3

type Worker struct {
	cfg  *Config
	mail *MailAPIClient
	llm  *LMStudioClient

	// now is the clock deferrals are measured on; nil means time.Now.
	now func() time.Time
	// deferrals holds messages whose last attempt failed on their own
	// content. Fast rounds skip one until its retryAt; complete rounds retry
	// all of them and start the map afresh. Rounds run one at a time on one
	// goroutine, so it needs no lock. nil is an empty map.
	deferrals map[int64]deferral
}

// deferral is a message whose last attempt failed for a reason of its own,
// not an upstream outage.
type deferral struct {
	attempts int
	retryAt  time.Time
}

// firstRetryDelay is how long a fast round leaves a failed message alone
// before trying it again; each further failure doubles it, up to the complete
// round interval, since a complete round retries it anyway.
const firstRetryDelay = 5 * time.Minute

// roundKind is which of the two rounds a pass belongs to.
type roundKind string

const (
	// roundFast reads only the messages queued for the pipeline
	// (annotation_pass_required, epistula-database migration 022), so it costs
	// what is queued rather than what is stored.
	roundFast roundKind = "fast"
	// roundComplete sweeps for everything this model has not annotated or
	// classified, queued or not.
	roundComplete roundKind = "complete"
)

// errPassQueueUnavailable is a fast round finding that epistula-api has no
// annotation pass queue; the caller runs a complete round instead.
var errPassQueueUnavailable = errors.New("epistula-api has no annotation pass queue (epistula-database migration 022)")

type RunStats struct {
	Scanned   int64
	Skipped   int64
	Annotated int64
	// Classified counts archive classifications written, by either pass.
	Classified int64
	Failed     int64
	// InfraFailed counts messages that failed because an UPSTREAM was
	// unavailable rather than because of anything about the message. It is
	// reported separately so a pass that annotated nothing during an outage
	// cannot look like a pass that found nothing to do (RA6X-042).
	InfraFailed int64
	// Deferred counts queued messages a fast round left alone because they
	// failed recently.
	Deferred int64
	// QueuePruned and QueueRemaining are the round's closing prune: markers
	// cleared for finished messages, and markers still queued in scope. Valid
	// when QueueKnown.
	QueuePruned    int64
	QueueRemaining int64
	QueueKnown     bool
}

func NewWorker(cfg *Config) (*Worker, error) {
	mail, err := NewMailAPIClient(cfg.MailAPI)
	if err != nil {
		return nil, fmt.Errorf("epistula-api client: %w", err)
	}
	llm, err := NewLMStudioClient(cfg.LMStudio)
	if err != nil {
		return nil, fmt.Errorf("lmstudio client: %w", err)
	}
	return &Worker{cfg: cfg, mail: mail, llm: llm}, nil
}

// RunOnce runs one complete round: the sweep run-once and every
// worker.complete_round_interval_seconds of serve perform.
func (w *Worker) RunOnce(ctx context.Context) (RunStats, error) {
	return w.runRound(ctx, roundComplete)
}

// runRound runs one annotation pass and, with [classify] enabled, one
// classification pass after it, then clears the queue markers of what is now
// finished.
//
// The annotation pass streams this model's unannotated messages; for a mailbox
// with archive categories it asks for the annotation and the archive category
// in one request. The classification pass then streams, per such mailbox, the
// messages with no current classification — ones annotated before
// classification was enabled, or classified against a category since retired
// — and asks for the category alone. A fast round asks both only of the
// queued messages.
//
// The prune runs after either kind of round: a complete round finishes queued
// messages too, and a message that arrived finished (an IMAP copy of one
// already done) is cleared by it without any work. A fast round against a
// epistula-api without the queue returns errPassQueueUnavailable before doing
// anything.
func (w *Worker) runRound(ctx context.Context, round roundKind) (RunStats, error) {
	if round == roundComplete {
		w.deferrals = nil
	}
	vocab := newVocabCache(w.mail)
	stats, err := w.annotatePass(ctx, vocab, round)
	if err == nil && w.cfg.Classify.Enabled {
		var cstats RunStats
		cstats, err = w.classifyPass(ctx, vocab, stats.Annotated, round)
		stats.Scanned += cstats.Scanned
		stats.Skipped += cstats.Skipped
		stats.Classified += cstats.Classified
		stats.Failed += cstats.Failed
		stats.InfraFailed += cstats.InfraFailed
		stats.Deferred += cstats.Deferred
	}
	if round == roundFast && isPassQueueUnavailable(err) {
		return stats, errPassQueueUnavailable
	}
	if err == nil && vocab.err != nil {
		// Annotation went ahead without the category; the next pass's
		// classification pass fills it in. The pass still reports the
		// failure, as an incomplete one must (RA6X-042).
		err = fmt.Errorf("archive categories unavailable, messages annotated without classification: %w", vocab.err)
	}
	if w.cfg.Worker.DryRun || ctx.Err() != nil {
		return stats, err
	}
	res, perr := w.mail.PrunePassQueue(ctx, PassPrune{
		Model:                 w.cfg.Worker.AnnotationModel,
		Mailbox:               w.cfg.MailAPI.Mailbox,
		RequireClassification: w.cfg.Classify.Enabled,
	})
	switch {
	case perr == nil:
		stats.QueuePruned, stats.QueueRemaining, stats.QueueKnown = res.Pruned, res.Remaining, true
		metricPassMarkersPruned.Add(res.Pruned)
		metricPassQueueRemaining.Store(res.Remaining)
	case isPassQueueUnavailable(perr):
		// A complete round does not need the queue.
	case err == nil:
		err = fmt.Errorf("prune annotation pass queue: %w", perr)
	}
	return stats, err
}

// deferred reports whether a fast round should leave msgID alone for now.
func (w *Worker) deferred(msgID int64) bool {
	d, ok := w.deferrals[msgID]
	return ok && w.clock().Before(d.retryAt)
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// noteOutcome records how processing msgID went. Success forgets it. A
// failure of the message's own (not an upstream outage, which says nothing
// about the message and is handled by aborting the pass) defers it: 5
// minutes, doubling per failure, never past the complete round interval.
func (w *Worker) noteOutcome(msgID int64, err error) {
	if err == nil {
		delete(w.deferrals, msgID)
		return
	}
	if isInfraFailure(err) || isCanceled(err) {
		return
	}
	if w.deferrals == nil {
		w.deferrals = map[int64]deferral{}
	}
	d := w.deferrals[msgID]
	d.attempts++
	delay := firstRetryDelay << min(d.attempts-1, 16)
	if limit := time.Duration(w.cfg.Worker.CompleteRoundIntervalSec) * time.Second; limit > 0 && delay > limit {
		delay = limit
	}
	d.retryAt = w.clock().Add(delay)
	w.deferrals[msgID] = d
}

// isCanceled reports whether err is the pass being canceled, which is not the
// message's fault either.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (w *Worker) annotatePass(ctx context.Context, vocab *vocabCache, round roundKind) (RunStats, error) {
	var stats RunStats
	errStop := errors.New("batch limit reached")
	// Let epistula-api skip what this model already annotated (server-side
	// efficiency); shouldSkip below is the belt-and-suspenders fallback for a
	// epistula-api that doesn't support the filter.
	notAnnotatedBy := ""
	if w.cfg.Worker.SkipAnnotated {
		notAnnotatedBy = w.cfg.Worker.AnnotationModel
	}
	consecutiveInfra := 0
	filter := exportFilter{NotAnnotatedBy: notAnnotatedBy, PassRequired: round == roundFast}
	err := w.exportResuming(ctx, filter, func(msg Message) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stats.Scanned++
		metricMessagesScanned.Add(1)
		if w.shouldSkip(msg) {
			stats.Skipped++
			metricMessagesSkipped.Add(1)
			consecutiveInfra = 0
			return nil
		}
		if round == roundFast && w.deferred(msg.ID) {
			stats.Deferred++
			return nil
		}
		var v *archiveVocabulary
		if w.cfg.Classify.Enabled {
			v = vocab.get(ctx, msg.Mailbox)
		}
		classified, err := w.processMessage(ctx, msg, v)
		w.noteOutcome(msg.ID, err)
		if classified {
			stats.Classified++
			metricMessagesClassified.Add(1)
		}
		if err != nil {
			stats.Failed++
			metricMessagesFailed.Add(1)
			slog.Warn("message annotation failed", "message_id", msg.ID, "err", err)
			if w.cfg.Worker.ExitOnMessageFailure {
				return err
			}
			// Infrastructure failures (either upstream unreachable, throttling
			// us, failing, or rejecting our token) recur for every message —
			// abort the pass after a short run of them instead of churning the
			// whole corpus. Per-message content errors reset the counter and
			// continue. See R-037; the set of statuses that counts is
			// RA6X-042.
			if isInfraFailure(err) {
				stats.InfraFailed++
				consecutiveInfra++
				if consecutiveInfra >= consecutiveInfraFailureAbort {
					return fmt.Errorf("aborting pass after %d consecutive infrastructure failures: %w", consecutiveInfra, err)
				}
			} else {
				consecutiveInfra = 0
			}
			return nil
		}
		consecutiveInfra = 0
		stats.Annotated++
		metricMessagesAnnotated.Add(1)
		if w.cfg.Worker.BatchLimit > 0 && stats.Annotated >= int64(w.cfg.Worker.BatchLimit) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	// A pass that hit infrastructure failures without reaching the abort
	// threshold is still an incomplete pass, and run-once's exit status and
	// the service's last-error state have to say so (RA6X-042). Reporting
	// success here is what let a systemic outage look like a quiet night.
	if err == nil && stats.InfraFailed > 0 {
		err = fmt.Errorf("pass incomplete: %d message(s) left unannotated by upstream failures", stats.InfraFailed)
	}
	return stats, err
}

func (w *Worker) shouldSkip(msg Message) bool {
	if !w.cfg.Worker.SkipAnnotated {
		return false
	}
	for _, ann := range msg.Annotations {
		if ann.Model == w.cfg.Worker.AnnotationModel {
			return true
		}
	}
	return false
}

// processMessage annotates one message and, when v is non-nil, classifies it
// in the same request. classified reports whether a classification was
// stored.
func (w *Worker) processMessage(ctx context.Context, msg Message, v *archiveVocabulary) (classified bool, err error) {
	var (
		ann  LLMAnnotation
		resp *chatResponse
	)
	err = w.withSmallerBodies(ctx, msg.ID, w.cfg.Worker.MaxBodyChars, func(maxBody int) error {
		if v != nil {
			prompt := buildAnnotateClassifyPrompt(w.cfg, msg, v, maxBody)
			return w.withRetry(ctx, func() error {
				var err error
				ann, resp, err = w.llm.AnnotateAndClassify(ctx, prompt, v)
				return err
			})
		}
		prompt := buildPrompt(w.cfg, msg, maxBody)
		return w.withRetry(ctx, func() error {
			var err error
			ann, resp, err = w.llm.Annotate(ctx, prompt)
			return err
		})
	})
	if err != nil {
		return false, err
	}
	put := normalizeAnnotation(w.cfg, ann)
	if resp != nil && resp.Usage != nil {
		put.TokensIn = &resp.Usage.PromptTokens
		put.TokensOut = &resp.Usage.CompletionTokens
	}
	if w.cfg.Worker.DryRun {
		slog.Info("dry-run annotation",
			"message_id", msg.ID,
			"model", put.Model,
			"tags", put.Tags,
			"category", valueOrEmpty(put.Category),
			"archive_category", ann.ArchiveCategory,
			"summary_chars", len(valueOrEmpty(put.Summary)))
		return false, nil
	}
	// The PUT gets the same bounded retry as inference does (RA6X-042). A
	// epistula-api 429 or 503 used to fail the message outright, so a brief
	// annotation-store hiccup burned the LLM work that had already been done
	// for it.
	if err := w.withRetry(ctx, func() error {
		return w.mail.PutAnnotation(ctx, msg.ID, put)
	}); err != nil {
		return false, err
	}
	if v == nil || ann.ArchiveConfidence == nil {
		return false, nil
	}
	// Written after the annotation, so a message whose classification PUT
	// fails is still annotated and is picked up by the classification pass
	// rather than re-annotated.
	cput := ClassificationPut{Category: ann.ArchiveCategory, Confidence: *ann.ArchiveConfidence, Model: w.cfg.Worker.AnnotationModel}
	if err := w.withRetry(ctx, func() error {
		return w.mail.PutClassification(ctx, msg.ID, cput)
	}); err != nil {
		return false, fmt.Errorf("classification: %w", err)
	}
	return true, nil
}

// classifyPass classifies, mailbox by mailbox, every message with no current
// classification. processed is how many messages the annotation pass already
// handled, which counts against worker.batch_limit.
func (w *Worker) classifyPass(ctx context.Context, vocab *vocabCache, processed int64, round roundKind) (RunStats, error) {
	var stats RunStats
	if w.cfg.Worker.BatchLimit > 0 && processed >= int64(w.cfg.Worker.BatchLimit) {
		return stats, nil
	}
	mailboxes := []string{w.cfg.MailAPI.Mailbox}
	if w.cfg.MailAPI.Mailbox == "" {
		names, err := w.mail.Mailboxes(ctx)
		if err != nil {
			return stats, fmt.Errorf("list mailboxes for classification: %w", err)
		}
		mailboxes = names
	}
	errStop := errors.New("batch limit reached")
	consecutiveInfra := 0
	for _, mailbox := range mailboxes {
		v := vocab.get(ctx, mailbox)
		if v == nil {
			continue
		}
		filter := exportFilter{Mailbox: mailbox, NotClassified: true, PassRequired: round == roundFast}
		err := w.exportResuming(ctx, filter, func(msg Message) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			stats.Scanned++
			metricMessagesScanned.Add(1)
			if round == roundFast && w.deferred(msg.ID) {
				stats.Deferred++
				return nil
			}
			err := w.classifyMessage(ctx, msg, v)
			w.noteOutcome(msg.ID, err)
			if err != nil {
				stats.Failed++
				metricMessagesFailed.Add(1)
				slog.Warn("message classification failed", "message_id", msg.ID, "err", err)
				if w.cfg.Worker.ExitOnMessageFailure {
					return err
				}
				if isInfraFailure(err) {
					stats.InfraFailed++
					consecutiveInfra++
					if consecutiveInfra >= consecutiveInfraFailureAbort {
						return fmt.Errorf("aborting classification pass after %d consecutive infrastructure failures: %w", consecutiveInfra, err)
					}
				} else {
					consecutiveInfra = 0
				}
				return nil
			}
			consecutiveInfra = 0
			if !w.cfg.Worker.DryRun {
				stats.Classified++
				metricMessagesClassified.Add(1)
			}
			processed++
			if w.cfg.Worker.BatchLimit > 0 && processed >= int64(w.cfg.Worker.BatchLimit) {
				return errStop
			}
			return nil
		})
		if errors.Is(err, errStop) {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}
	}
	if stats.InfraFailed > 0 {
		return stats, fmt.Errorf("classification pass incomplete: %d message(s) left unclassified by upstream failures", stats.InfraFailed)
	}
	return stats, nil
}

func (w *Worker) classifyMessage(ctx context.Context, msg Message, v *archiveVocabulary) error {
	var cl LLMClassification
	err := w.withSmallerBodies(ctx, msg.ID, w.cfg.Classify.MaxBodyChars, func(maxBody int) error {
		prompt := buildClassifyPrompt(w.cfg, msg, v, maxBody)
		return w.withRetry(ctx, func() error {
			var err error
			cl, _, err = w.llm.Classify(ctx, prompt, v)
			return err
		})
	})
	if err != nil {
		return err
	}
	if w.cfg.Worker.DryRun {
		slog.Info("dry-run classification", "message_id", msg.ID,
			"archive_category", cl.ArchiveCategory, "confidence", *cl.ArchiveConfidence)
		return nil
	}
	put := ClassificationPut{Category: cl.ArchiveCategory, Confidence: *cl.ArchiveConfidence, Model: w.cfg.Worker.AnnotationModel}
	return w.withRetry(ctx, func() error {
		return w.mail.PutClassification(ctx, msg.ID, put)
	})
}

// maxStreamReconnects bounds how many times one pass reopens an export that
// was cut off mid-stream.
const maxStreamReconnects = 50

// exportResuming streams an export and, when the stream is cut off after it
// delivered rows, opens a fresh one at once instead of failing the pass.
//
// A buffering reverse proxy can read far ahead of a slow model worker. Its
// idle backend connection may then hit the route timeout while the worker
// drains buffered rows. The worker eventually reads an unexpected EOF, even
// though the delivered rows were valid.
//
// Reopening resumes rather than repeats only because the filter excludes
// finished work (not_annotated_by, not_classified). Without one, a new stream
// would start again at the newest message, so the error stands. A stream that
// is cut before delivering anything also stands: that is an upstream
// failure, not a proxy trimming a long stream.
func (w *Worker) exportResuming(ctx context.Context, f exportFilter, handle func(Message) error) error {
	resumable := f.NotAnnotatedBy != "" || f.NotClassified
	for reconnects := 0; ; reconnects++ {
		delivered := false
		err := w.mail.ExportFiltered(ctx, w.cfg.MailAPI, f, func(msg Message) error {
			delivered = true
			return handle(msg)
		})
		if err == nil || !resumable || !delivered || ctx.Err() != nil ||
			!isStreamCut(err) || reconnects >= maxStreamReconnects {
			return err
		}
		metricStreamReconnects.Add(1)
		slog.Info("export stream was cut off; reopening it", "reconnect", reconnects+1, "err", err)
	}
}

// isStreamCut reports whether err is an export stream that ended mid-way: a
// truncated body or a reset connection. The idle watchdog's error (the server
// went quiet) is not one; that is a stalled upstream and fails the pass.
func isStreamCut(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// minBodyBudget is the smallest message body a context overflow shrinks a
// prompt to. Below it there is too little of the message left to judge.
const minBodyBudget = 2000

// bodyBudgets is the sequence of body sizes to try after a context overflow:
// the configured size, then halved down to minBodyBudget.
func bodyBudgets(start int) []int {
	out := []int{start}
	for cur := start; cur > minBodyBudget; {
		cur = max(cur/2, minBodyBudget)
		out = append(out, cur)
	}
	return out
}

// withSmallerBodies runs attempt with the configured body budget and, while the
// model refuses the prompt as too long for its context window, again with half
// as much of the body, down to minBodyBudget. A message whose prompt overflows
// even then fails as a content error. Anything other than an overflow ends the
// sequence at once.
//
// URL-heavy newsletters and non-Latin text run at about 1.3 characters a token,
// so a body budget that fits most mail can still overflow for them.
// They cost a retry instead of failing on every pass.
func (w *Worker) withSmallerBodies(ctx context.Context, messageID int64, start int, attempt func(maxBody int) error) error {
	budgets := bodyBudgets(start)
	var err error
	for i, budget := range budgets {
		err = attempt(budget)
		var overflow *ContextOverflowError
		if !errors.As(err, &overflow) {
			return err
		}
		metricContextOverflows.Add(1)
		if i+1 < len(budgets) {
			slog.Info("prompt exceeds the model's context window; retrying with less of the message",
				"message_id", messageID, "max_body_chars", budgets[i+1])
		}
	}
	return err
}

// vocabRefresh is how long a mailbox's category list is reused before it is
// fetched again. A backfill pass lasts days, and a re-imported list should
// take effect within minutes, not at the next pass or a restart.
const vocabRefresh = 10 * time.Minute

// vocabCache holds each mailbox's archive categories for a pass, refetching a
// list once it is vocabRefresh old.
type vocabCache struct {
	mail      *MailAPIClient
	now       func() time.Time
	byMailbox map[string]vocabEntry
	// err is the first fetch failure for a mailbox with no list to fall back
	// on. That mailbox is treated as having no categories until a fetch
	// succeeds, and RunOnce reports the failure.
	err error
}

type vocabEntry struct {
	v       *archiveVocabulary // nil: the mailbox has no categories
	fetched time.Time
}

func newVocabCache(mail *MailAPIClient) *vocabCache {
	return &vocabCache{mail: mail, now: time.Now, byMailbox: map[string]vocabEntry{}}
}

// get returns the mailbox's categories, or nil when it has none (or they
// could not be fetched).
//
// A refresh that fails keeps the list already held: classifying against a
// list minutes old is correct in all but a just-changed category, and epistula-api
// refuses a key that has been retired since, so a stale choice is dropped, not
// filed. Only a mailbox with no list at all counts as a failure of the pass.
func (c *vocabCache) get(ctx context.Context, mailbox string) *archiveVocabulary {
	if mailbox == "" {
		return nil
	}
	now := c.now()
	entry, held := c.byMailbox[mailbox]
	if held && now.Sub(entry.fetched) < vocabRefresh {
		return entry.v
	}
	cats, err := c.mail.ArchiveCategories(ctx, mailbox)
	if err != nil {
		if held && entry.v != nil {
			slog.Warn("archive categories refresh failed; keeping the list already held",
				"mailbox", mailbox, "age", now.Sub(entry.fetched).Round(time.Second), "err", err)
			// Try again next time rather than on every message.
			c.byMailbox[mailbox] = vocabEntry{v: entry.v, fetched: now}
			return entry.v
		}
		slog.Warn("archive categories unavailable; annotating without classification",
			"mailbox", mailbox, "err", err)
		if c.err == nil {
			c.err = err
		}
		c.byMailbox[mailbox] = vocabEntry{fetched: now}
		return nil
	}
	var v *archiveVocabulary
	if len(cats) > 0 {
		v = &archiveVocabulary{Mailbox: mailbox, Categories: cats}
	}
	if held && entry.v != nil && v != nil && len(entry.v.Categories) != len(v.Categories) {
		slog.Info("archive categories changed", "mailbox", mailbox,
			"before", len(entry.v.Categories), "after", len(v.Categories))
	}
	c.byMailbox[mailbox] = vocabEntry{v: v, fetched: now}
	return v
}

// withRetry runs op under the configured attempt budget, honouring an
// upstream's Retry-After when it sent one (RA6X-042).
//
// Not every failure is worth another attempt: an authentication rejection will
// answer the same way however long we wait, so retrying it only burns the
// budget and delays the pass abort that is the correct response. Throttling,
// service failures and transport errors are retried; a per-message content
// failure is retried too, which is what RetryAttempts has always been for.
//
// A canceled context (SIGTERM) must not burn the remaining attempts or wait
// out the backoff — including a long Retry-After — so cancellation is checked
// before each wait and the wait itself is cancellable (R-055).
func (w *Worker) withRetry(ctx context.Context, op func() error) error {
	attempts := w.cfg.Worker.RetryAttempts + 1
	var err error
	for i := 0; i < attempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isRetryable(err) {
			return err
		}
		if i+1 < attempts {
			if werr := sleepCtx(ctx, retryDelay(err, w.cfg.RetryBackoffDuration())); werr != nil {
				return werr
			}
		}
	}
	return err
}

// sleepCtx waits for d or until ctx is canceled, whichever comes first. It
// returns ctx.Err() on cancellation so a retry backoff can short-circuit on
// shutdown, and nil after a full sleep (R-055).
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type atomicRunState struct {
	running      atomic.Bool
	lastStarted  atomic.Int64
	lastFinished atomic.Int64
	lastError    atomic.Value
}

func (s *atomicRunState) setError(err error) {
	if err == nil {
		s.lastError.Store("")
	} else {
		s.lastError.Store(err.Error())
	}
}

func (s *atomicRunState) errorString() string {
	v := s.lastError.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}
