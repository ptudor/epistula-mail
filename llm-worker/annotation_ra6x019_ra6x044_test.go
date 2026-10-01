package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnnotationValuesMustHaveTheRightType is the RA6X-019 regression.
//
// The decoder checked key PRESENCE, not value type. JSON null decodes into a
// zero-valued Go string or a nil slice without an error, so an all-null object
// passed every check, normalizeAnnotation filled in its fallbacks, and the
// worker PUT the result under this model's name. Every later pass then skipped
// the message as already annotated, hiding the model-output failure for good.
func TestAnnotationValuesMustHaveTheRightType(t *testing.T) {
	rejected := map[string]string{
		"all null":            `{"summary":null,"tags":null,"category":null}`,
		"null summary":        `{"summary":null,"tags":["x"],"category":"other"}`,
		"null tags":           `{"summary":"s","tags":null,"category":"other"}`,
		"null category":       `{"summary":"s","tags":["x"],"category":null}`,
		"numeric summary":     `{"summary":42,"tags":["x"],"category":"other"}`,
		"object summary":      `{"summary":{"text":"s"},"tags":["x"],"category":"other"}`,
		"boolean category":    `{"summary":"s","tags":["x"],"category":true}`,
		"string tags":         `{"summary":"s","tags":"x","category":"other"}`,
		"object tags":         `{"summary":"s","tags":{"a":1},"category":"other"}`,
		"non-string tag":      `{"summary":"s","tags":[1,2],"category":"other"}`,
		"whitespace summary":  `{"summary":"   \n\t ","tags":["x"],"category":"other"}`,
		"whitespace category": `{"summary":"s","tags":["x"],"category":"  "}`,
		"whitespace tag":      `{"summary":"s","tags":["ok","  "],"category":"other"}`,
		"missing key":         `{"summary":"s","tags":["x"]}`,
		"empty object":        `{}`,
	}
	for name, body := range rejected {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAnnotation(body, ""); err == nil {
				t.Fatalf("%s was accepted as a completed annotation", name)
			}
		})
	}

	// Valid output still decodes, with and without tags.
	for _, body := range []string{
		`{"summary":"a real summary","tags":["receipt"],"category":"finance"}`,
		`{"summary":"a real summary","tags":[],"category":"finance"}`,
	} {
		ann, err := decodeAnnotation(body, "")
		if err != nil {
			t.Fatalf("valid output %s was rejected: %v", body, err)
		}
		if ann.Summary != "a real summary" || ann.Category != "finance" {
			t.Errorf("decoded %#v", ann)
		}
	}
}

// TestSalvageRejectsMalformedCandidates pins that the same rule guards the
// path that pulls an object out of model prose, where a half-written draft is
// exactly the kind of object most likely to carry a null.
func TestSalvageRejectsMalformedCandidates(t *testing.T) {
	// A malformed draft followed by a valid answer: the valid one wins.
	content := `<think>{"summary":null,"tags":null,"category":null}</think>` +
		`{"summary":"the real answer","tags":["x"],"category":"other"}`
	ann, err := decodeAnnotation(content, "")
	if err != nil {
		t.Fatalf("decodeAnnotation: %v", err)
	}
	if ann.Summary != "the real answer" {
		t.Errorf("summary = %q, want the valid object", ann.Summary)
	}

	// A valid draft followed by a malformed answer: the last VALID object is
	// what the R-034 rule accepts, and it is still a real annotation.
	content = `<think>{"summary":"a draft","tags":["x"],"category":"other"}</think>` +
		`{"summary":null,"tags":null,"category":null}`
	if ann, err := decodeAnnotation(content, ""); err != nil {
		t.Fatalf("decodeAnnotation: %v", err)
	} else if ann.Summary != "a draft" {
		t.Errorf("summary = %q, want the last valid object", ann.Summary)
	}

	// Nothing valid anywhere: an error, so the retry/failure policy runs and
	// no skip marker is written.
	content = `<think>{"summary":null,"tags":null,"category":null}</think>` +
		`{"nope":1}`
	if _, err := decodeAnnotation(content, ""); err == nil {
		t.Fatal("a response with no valid annotation was accepted")
	}
}

// TestNormalizationStaysWithinTheConfiguredVocabulary is the RA6X-044
// regression.
func TestNormalizationStaysWithinTheConfiguredVocabulary(t *testing.T) {
	// A vocabulary with no "other" at all.
	cfg := validatableDefault()
	cfg.Worker.Categories = []string{"work", "personal"}
	cfg.Worker.AllowedTags = []string{}
	put := normalizeAnnotation(cfg, LLMAnnotation{
		Summary: "s", Tags: []string{"unmapped"}, Category: "not-in-vocabulary",
	})
	if got := valueOrEmpty(put.Category); got != "work" {
		t.Errorf("category = %q; the fallback must come from the configured vocabulary, not a hard-coded \"other\"", got)
	}

	// A restrictive allowed-tags list: the empty-tags fallback must not smuggle
	// the category past it.
	cfg = validatableDefault()
	cfg.Worker.Categories = []string{"work", "personal"}
	cfg.Worker.AllowedTags = []string{"approved"}
	put = normalizeAnnotation(cfg, LLMAnnotation{
		Summary: "s", Tags: []string{"rejected-tag"}, Category: "work",
	})
	for _, tag := range put.Tags {
		if tag != "approved" {
			t.Errorf("tag %q is outside the configured allowed_tags", tag)
		}
	}
	if len(put.Tags) != 0 {
		t.Errorf("tags = %v; with nothing allowed the annotation must go out with none", put.Tags)
	}

	// With the category itself allowed, the fallback is used.
	cfg.Worker.AllowedTags = []string{"work"}
	put = normalizeAnnotation(cfg, LLMAnnotation{
		Summary: "s", Tags: []string{"rejected-tag"}, Category: "work",
	})
	if len(put.Tags) != 1 || put.Tags[0] != "work" {
		t.Errorf("tags = %v, want [work]", put.Tags)
	}

	// Every tag that does go out is inside the contract's byte limit, fallback
	// included.
	cfg = validatableDefault()
	cfg.Worker.Categories = []string{strings.Repeat("z", 300)}
	cfg.Worker.AllowedTags = []string{}
	put = normalizeAnnotation(cfg, LLMAnnotation{Summary: "s", Tags: nil, Category: "nope"})
	if c := valueOrEmpty(put.Category); len(c) > apiMaxCategoryBytes {
		t.Errorf("category is %d bytes, over the %d-byte contract", len(c), apiMaxCategoryBytes)
	}
	for _, tag := range put.Tags {
		if len(tag) > apiMaxTagBytes {
			t.Errorf("tag is %d bytes, over the %d-byte contract", len(tag), apiMaxTagBytes)
		}
	}
}

// TestSerializedAnnotationFitsTheAPICap pins that the PUT body itself stays
// under epistula-api's cap after every fallback, including a summary whose
// characters cost several bytes each once JSON-escaped.
func TestSerializedAnnotationFitsTheAPICap(t *testing.T) {
	cfg := validatableDefault()
	// A character limit that looks modest but whose JSON encoding is not: each
	// of these runes is 3 UTF-8 bytes, and a control character escapes to six.
	cfg.Worker.MaxSummaryChars = 200000
	for name, summary := range map[string]string{
		"ascii":     strings.Repeat("a", 200000),
		"multibyte": strings.Repeat("日", 200000),
		"escaped":   strings.Repeat("\u0001", 200000),
	} {
		t.Run(name, func(t *testing.T) {
			put := normalizeAnnotation(cfg, LLMAnnotation{
				Summary: summary, Tags: []string{"x"}, Category: "other",
			})
			body, err := json.Marshal(&put)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if len(body) > apiMaxAnnotationBytes {
				t.Errorf("serialized PUT is %d bytes, over epistula-api's %d-byte cap",
					len(body), apiMaxAnnotationBytes)
			}
			if put.Summary != nil && *put.Summary == "" {
				t.Error("the summary was emptied rather than shortened")
			}
		})
	}
}

// TestAnnotationModelIsCanonicalizedOnce is the skip-loop half of RA6X-044.
//
// epistula-api trims the model on write; the worker sent the untrimmed string and
// compared the untrimmed string, so an annotation_model with a stray space was
// stored under the trimmed name, never matched the skip check, and the whole
// corpus was re-annotated on every pass.
func TestAnnotationModelIsCanonicalizedOnce(t *testing.T) {
	cfg := validatableDefault()
	cfg.Worker.AnnotationModel = "  padded-model\n"
	cfg.deriveAnnotationModel()
	if cfg.Worker.AnnotationModel != "padded-model" {
		t.Fatalf("annotation_model = %q, want it canonicalized at load time", cfg.Worker.AnnotationModel)
	}

	// What the worker writes and what it compares against are now the same
	// string, so a second pass skips what the first one wrote.
	put := normalizeAnnotation(cfg, LLMAnnotation{Summary: "s", Tags: []string{"x"}, Category: "other"})
	if put.Model != "padded-model" {
		t.Errorf("PUT model = %q, want the canonical form", put.Model)
	}
	cfg.Worker.SkipAnnotated = true
	w := &Worker{cfg: cfg}
	stored := Message{Annotations: []Annotation{{Model: put.Model}}}
	if !w.shouldSkip(stored) {
		t.Error("a message annotated by this worker was not skipped on the next pass")
	}
}

// TestConfigRejectsUnproducibleVocabulary pins the configuration side: a
// vocabulary this worker could not emit under the epistula-api contract is a
// config error, not a per-message 422 discovered in production.
func TestConfigRejectsUnproducibleVocabulary(t *testing.T) {
	// A config that is otherwise complete, so the ONLY reason any case below
	// can be rejected is the vocabulary under test.
	base := func() *Config {
		cfg := validatableDefault()
		cfg.MailAPI.BaseURL = "https://mail.invalid"
		cfg.MailAPI.Token = "mapi_test"
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the baseline config is not valid, so this test proves nothing: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"allowed tag over the per-tag limit": func(c *Config) {
			c.Worker.AllowedTags = []string{strings.Repeat("t", 200)}
		},
		"category over the category limit": func(c *Config) {
			c.Worker.Categories = []string{strings.Repeat("c", 300)}
		},
		"empty category vocabulary": func(c *Config) {
			c.Worker.Categories = []string{}
		},
		"category that normalizes to nothing": func(c *Config) {
			c.Worker.Categories = []string{"---"}
		},
		"allowed tag that normalizes to nothing": func(c *Config) {
			c.Worker.AllowedTags = []string{"***"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate accepted %s; every message would be rejected 422 and re-annotated forever", name)
			}
		})
	}
}
