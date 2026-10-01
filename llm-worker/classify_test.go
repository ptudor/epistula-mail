package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testVocab = &archiveVocabulary{Mailbox: "jdoe", Categories: []ArchiveCategory{
	{Key: "finance/banking", Folder: "Archive/Finance/Banking", Description: "Bank statements\nand card alerts"},
	{Key: "travel", Folder: "Archive/Travel"},
}}

func TestArchiveInstructions(t *testing.T) {
	got := archiveInstructions(testVocab)
	for _, want := range []string{
		"- finance/banking: Bank statements and card alerts\n", // control characters flattened
		"- travel: Archive/Travel\n",                           // no description: the folder stands in
		"untrusted data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions lack %q:\n%s", want, got)
		}
	}
	format := classifyResponseFormat(testVocab)
	b, _ := json.Marshal(format)
	if !strings.Contains(string(b), `"enum":["finance/banking","travel"]`) {
		t.Fatalf("classification schema does not constrain the key: %s", b)
	}
	b, _ = json.Marshal(annotateClassifyResponseFormat(testVocab))
	for _, want := range []string{`"archive_category"`, `"archive_confidence"`, `"summary"`, `"required":["summary","tags","category","archive_category","archive_confidence"]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("combined schema lacks %s: %s", want, b)
		}
	}
}

func TestClassifyPromptBoundsTheBody(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Classify.MaxBodyChars = 10
	body := strings.Repeat("x", 100)
	p := BuildClassifyPrompt(cfg, Message{ID: 1, Mailbox: "jdoe", Folder: "INBOX", TextBody: &body}, testVocab)
	if strings.Contains(p.User, strings.Repeat("x", 11)) || !strings.Contains(p.User, truncationMarker) {
		t.Fatalf("classification prompt sent more than classify.max_body_chars:\n%s", p.User)
	}
	if !strings.Contains(p.System, "archive_category must be exactly one of the keys") {
		t.Fatal("classification prompt lacks the archive instructions")
	}
	combined := BuildAnnotateClassifyPrompt(cfg, Message{ID: 1, TextBody: &body}, testVocab)
	if !strings.HasPrefix(combined.System, BuildPrompt(cfg, Message{}).System) {
		t.Fatal("the combined prompt does not extend the annotation prompt")
	}
}

func TestDecodeArchiveAnswers(t *testing.T) {
	annotate := unmarshalAnnotationArchive(testVocab)
	ok := `{"summary":"a statement","tags":["bank"],"category":"finance","archive_category":"finance/banking","archive_confidence":0.83}`
	ann, err := annotate(ok)
	if err != nil {
		t.Fatalf("valid combined answer refused: %v", err)
	}
	if ann.ArchiveCategory != "finance/banking" || ann.ArchiveConfidence == nil || *ann.ArchiveConfidence != 0.83 {
		t.Fatalf("decoded %+v", ann)
	}
	for name, bad := range map[string]string{
		"key not listed":      `{"summary":"s","tags":[],"category":"c","archive_category":"shopping","archive_confidence":0.9}`,
		"confidence above 1":  `{"summary":"s","tags":[],"category":"c","archive_category":"travel","archive_confidence":1.2}`,
		"confidence null":     `{"summary":"s","tags":[],"category":"c","archive_category":"travel","archive_confidence":null}`,
		"confidence a string": `{"summary":"s","tags":[],"category":"c","archive_category":"travel","archive_confidence":"0.9"}`,
		"missing archive":     `{"summary":"s","tags":[],"category":"c"}`,
		"bad annotation":      `{"summary":"","tags":[],"category":"c","archive_category":"travel","archive_confidence":0.9}`,
	} {
		if _, err := annotate(bad); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
	classify := unmarshalClassification(testVocab)
	cl, err := classify(`{"archive_category":"travel","archive_confidence":0}`)
	if err != nil || cl.ArchiveCategory != "travel" || *cl.ArchiveConfidence != 0 {
		t.Fatalf("classification = %+v, %v", cl, err)
	}
	// The last valid object wins, as for annotations (R-034).
	got, err := decodeLastValid(`<think>{"archive_category":"finance/banking","archive_confidence":0.1}</think> {"archive_category":"travel","archive_confidence":0.7}`, "", classify)
	if err != nil || got.ArchiveCategory != "travel" {
		t.Fatalf("decodeLastValid = %+v, %v", got, err)
	}
}

// fakeStore is an in-memory epistula-api for worker passes: two mailboxes, only
// jdoe with archive categories, plus the LM Studio side.
type fakeStore struct {
	mu              sync.Mutex
	messages        map[int64]Message
	annotated       map[int64]bool
	classified      map[int64]string
	categoriesDown  bool
	schemasSeen     map[string]int
	classifyRejects int
	// queued is the annotation pass queue (epistula-database migration 022);
	// queueDown answers for it the way a epistula-api without it does.
	queued    map[int64]bool
	queueDown bool
	prunes    []PassPrune
	// garbled makes the model answer with something that is not JSON: a
	// failure of the message's own, not an upstream outage.
	garbled bool
	lmCalls int
}

// queueUnavailable writes epistula-api's own 503 for the missing queue.
func queueUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"title":"Service Unavailable","detail":"migration 022 missing"}`)
}

func (s *fakeStore) mailAPI(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/mailboxes":
			fmt.Fprint(w, `{"mailboxes":[{"name":"asmith"},{"name":"jdoe"}]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/archive-categories"):
			if s.categoriesDown {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"title":"Service Unavailable","detail":"migration 020 missing"}`)
				return
			}
			if strings.Contains(r.URL.Path, "/jdoe/") {
				fmt.Fprint(w, `{"categories":[{"key":"finance/banking","folder":"Archive/Finance/Banking"},{"key":"travel","folder":"Archive/Travel"}]}`)
			} else {
				fmt.Fprint(w, `{"categories":[]}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/export":
			q := r.URL.Query()
			if q.Get("pass_required") == "true" && s.queueDown {
				queueUnavailable(w)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			for id := int64(1); id <= int64(len(s.messages)); id++ {
				m := s.messages[id]
				if mb := q.Get("mailbox"); mb != "" && m.Mailbox != mb {
					continue
				}
				if q.Get("pass_required") == "true" && !s.queued[id] {
					continue
				}
				if q.Get("not_annotated_by") != "" && s.annotated[id] {
					continue
				}
				if q.Get("not_classified") == "true" && s.classified[id] != "" {
					continue
				}
				_ = enc.Encode(m)
			}
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/annotation"):
			var id int64
			fmt.Sscanf(r.URL.Path, "/v1/messages/%d/annotation", &id)
			s.annotated[id] = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/classification"):
			var id int64
			fmt.Sscanf(r.URL.Path, "/v1/messages/%d/classification", &id)
			var put ClassificationPut
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Errorf("decode classification: %v", err)
			}
			if s.messages[id].Mailbox != "jdoe" {
				s.classifyRejects++
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			if put.Model != "lmstudio:test" || put.Confidence != 0.75 {
				t.Errorf("classification put = %+v", put)
			}
			s.classified[id] = put.Category
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pass-required/prune":
			if s.queueDown {
				queueUnavailable(w)
				return
			}
			var p PassPrune
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("decode prune: %v", err)
			}
			s.prunes = append(s.prunes, p)
			var res PassPruneResult
			for id := range s.queued {
				m := s.messages[id]
				if p.Mailbox != "" && m.Mailbox != p.Mailbox {
					continue
				}
				// Only jdoe has archive categories.
				finished := s.annotated[id] && (!p.RequireClassification || m.Mailbox != "jdoe" || s.classified[id] != "")
				if finished {
					delete(s.queued, id)
					res.Pruned++
				} else {
					res.Remaining++
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		default:
			t.Errorf("unexpected epistula-api request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *fakeStore) lmStudio(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResponseFormat struct {
				JSONSchema struct {
					Name string `json:"name"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		name := req.ResponseFormat.JSONSchema.Name
		s.mu.Lock()
		s.schemasSeen[name]++
		s.lmCalls++
		garbled := s.garbled
		s.mu.Unlock()
		var answer string
		switch name {
		case "mail_annotation":
			answer = `{"summary":"s","tags":["t"],"category":"other"}`
		case "mail_annotation_archive":
			answer = `{"summary":"s","tags":["t"],"category":"other","archive_category":"travel","archive_confidence":0.75}`
		case "mail_archive_classification":
			answer = `{"archive_category":"finance/banking","archive_confidence":0.75}`
		default:
			t.Errorf("unexpected schema %q", name)
		}
		if garbled {
			answer = "not json"
		}
		resp := map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"content": answer},
			"finish_reason": "stop",
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func newClassifyWorker(t *testing.T, s *fakeStore) *Worker {
	t.Helper()
	api := httptest.NewServer(s.mailAPI(t))
	t.Cleanup(api.Close)
	lm := httptest.NewServer(s.lmStudio(t))
	t.Cleanup(lm.Close)
	cfg := DefaultConfig()
	cfg.MailAPI.BaseURL = api.URL
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = lm.URL + "/v1"
	cfg.Worker.AnnotationModel = "lmstudio:test"
	cfg.Worker.RetryAttempts = 0
	cfg.Classify.Enabled = true
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func newFakeStore() *fakeStore {
	s := &fakeStore{
		messages:    map[int64]Message{},
		annotated:   map[int64]bool{},
		classified:  map[int64]string{},
		schemasSeen: map[string]int{},
		queued:      map[int64]bool{},
	}
	body := "text"
	for id := int64(1); id <= 4; id++ {
		mb := "jdoe"
		if id == 4 {
			mb = "asmith"
		}
		s.messages[id] = Message{ID: id, UID: id, Mailbox: mb, Folder: "INBOX", Flags: []string{}, TextBody: &body}
	}
	// Message 3 was annotated before classification existed.
	s.annotated[3] = true
	return s
}

// TestRunOnceAnnotatesAndClassifies: new mail in a mailbox with categories is
// annotated and classified in ONE request; a message annotated earlier is
// classified alone in the second pass; a mailbox without categories is only
// annotated, and never asked for a category epistula-api would refuse.
func TestRunOnceAnnotatesAndClassifies(t *testing.T) {
	s := newFakeStore()
	w := newClassifyWorker(t, s)
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Annotated != 3 || stats.Classified != 3 || stats.Failed != 0 {
		t.Fatalf("stats = %+v; want 3 annotated, 3 classified", stats)
	}
	if s.classified[1] != "travel" || s.classified[2] != "travel" {
		t.Fatalf("combined classifications = %v", s.classified)
	}
	if s.classified[3] != "finance/banking" {
		t.Fatalf("the pre-annotated message was classified %q, want the classification-only answer", s.classified[3])
	}
	if s.classified[4] != "" || s.classifyRejects != 0 {
		t.Fatal("a mailbox with no categories was classified")
	}
	if s.schemasSeen["mail_annotation_archive"] != 2 || s.schemasSeen["mail_archive_classification"] != 1 ||
		s.schemasSeen["mail_annotation"] != 1 {
		t.Fatalf("requests by schema = %v", s.schemasSeen)
	}

	// Nothing left: a second pass makes no model requests.
	before := s.schemasSeen["mail_annotation_archive"] + s.schemasSeen["mail_archive_classification"] + s.schemasSeen["mail_annotation"]
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := s.schemasSeen["mail_annotation_archive"] + s.schemasSeen["mail_archive_classification"] + s.schemasSeen["mail_annotation"]
	if after != before {
		t.Fatalf("a pass over finished work made %d model request(s)", after-before)
	}
}

// TestRunOnceWithoutCategoriesService: with epistula-api's archive endpoints
// unavailable, annotation still proceeds, but the pass reports the failure.
func TestRunOnceWithoutCategoriesService(t *testing.T) {
	s := newFakeStore()
	s.categoriesDown = true
	w := newClassifyWorker(t, s)
	stats, err := w.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "archive categories unavailable") {
		t.Fatalf("err = %v; want the classification outage reported", err)
	}
	if stats.Annotated != 3 || stats.Classified != 0 {
		t.Fatalf("stats = %+v; want annotation to go ahead unclassified", stats)
	}
}

// TestRunOnceClassifyDisabled: with [classify] off the worker behaves exactly
// as before — no category endpoint, no archive schema.
func TestRunOnceClassifyDisabled(t *testing.T) {
	s := newFakeStore()
	w := newClassifyWorker(t, s)
	w.cfg.Classify.Enabled = false
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Classified != 0 || s.schemasSeen["mail_annotation"] != 3 || len(s.schemasSeen) != 1 {
		t.Fatalf("stats = %+v, schemas = %v", stats, s.schemasSeen)
	}
}

func TestClassifyConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MailAPI.Token = "x"
	cfg.Worker.AnnotationModel = "m"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Classify.Enabled {
		t.Fatal("classification is on by default")
	}
	cfg.Classify.MaxBodyChars = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("classify.max_body_chars = 0 accepted")
	}
}

// TestVocabCacheRefreshes: a mailbox's category list is re-fetched once it is
// vocabRefresh old, so a re-import takes effect during a days-long pass, and a
// failed refresh keeps the list already held instead of failing the pass.
func TestVocabCacheRefreshes(t *testing.T) {
	var mu sync.Mutex
	keys := []string{"travel"}
	down := false
	fetches := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var cats []string
		for _, k := range keys {
			cats = append(cats, fmt.Sprintf(`{"key":%q,"folder":"Archive/%s"}`, k, k))
		}
		fmt.Fprintf(w, `{"categories":[%s]}`, strings.Join(cats, ","))
	}))
	defer api.Close()
	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: api.URL, Token: "mapi_test", RequestTimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	c := newVocabCache(client)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	if v := c.get(ctx, "jdoe"); v == nil || len(v.Categories) != 1 {
		t.Fatalf("first get = %+v", v)
	}
	mu.Lock()
	keys = []string{"travel", "shopping/electronics/digikey"}
	mu.Unlock()
	clock = clock.Add(vocabRefresh - time.Second)
	if v := c.get(ctx, "jdoe"); len(v.Categories) != 1 || fetches != 1 {
		t.Fatalf("within the refresh interval: %d categories after %d fetches; want the cached 1 after 1", len(v.Categories), fetches)
	}
	clock = clock.Add(2 * time.Second)
	if v := c.get(ctx, "jdoe"); len(v.Categories) != 2 || !v.has("shopping/electronics/digikey") {
		t.Fatalf("after the interval: %+v; want the re-imported list", v)
	}

	mu.Lock()
	down = true
	mu.Unlock()
	clock = clock.Add(vocabRefresh)
	if v := c.get(ctx, "jdoe"); v == nil || len(v.Categories) != 2 {
		t.Fatalf("failed refresh: %+v; want the list already held", v)
	}
	if c.err != nil {
		t.Fatalf("a failed refresh with a list held failed the pass: %v", c.err)
	}
	// And it does not retry on every message: the next attempt waits a full
	// interval.
	before := fetches
	c.get(ctx, "jdoe")
	if fetches != before {
		t.Fatal("a failed refresh is retried on every message")
	}
}
