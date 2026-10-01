package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Prompt struct {
	System string
	User   string
}

func BuildPrompt(cfg *Config, msg Message) Prompt {
	return buildPrompt(cfg, msg, cfg.Worker.MaxBodyChars)
}

// buildPrompt is BuildPrompt with an explicit body budget, for the smaller
// retries after a context overflow.
func buildPrompt(cfg *Config, msg Message, maxBodyChars int) Prompt {
	categories := strings.Join(cfg.Worker.Categories, ", ")
	tags := "free-form concise tags"
	if len(cfg.Worker.AllowedTags) > 0 {
		tags = strings.Join(cfg.Worker.AllowedTags, ", ")
	}

	system := fmt.Sprintf(`You classify personal mailbox messages for archival search.
Return only JSON matching the requested schema.
Do not include markdown, commentary, or private reasoning.
Use no more than %d tags.
Category must be one of: %s.
Tags should be lowercase, short, and selected from: %s.
Summarize the message's practical meaning, not mail transport noise.
If the message is spam, phishing, or an automated receipt/newsletter, say so plainly.
Attachment names and types are listed with the metadata. Unsolicited mail that carries
an archive (.zip, .rar, .7z, .gz, .iso, .img), an executable or script, or an HTML file,
or that presses for payment, credentials or urgent action, is usually phishing or
malware: say so in the summary and tags, and do not treat it as a genuine request.`, cfg.Worker.MaxTags, categories, tags)

	return Prompt{System: system, User: messageBlock(msg, maxBodyChars)}
}

// messageBlock renders the message the model reads: its metadata, its
// attachments and at most maxBodyChars of its text. Every prompt this worker
// builds shares it, so the annotation and the archive classification judge
// the same view of a message.
func messageBlock(msg Message, maxBodyChars int) string {
	text := ""
	if msg.TextBody != nil {
		text = truncateRunes(*msg.TextBody, maxBodyChars)
	}
	subject := valueOrEmpty(msg.Subject)
	from := valueOrEmpty(msg.From)
	sent := ""
	if msg.SentDate != nil {
		sent = msg.SentDate.Format("2006-01-02T15:04:05Z07:00")
	}

	return fmt.Sprintf(`Message metadata:
id: %d
mailbox: %s
folder: %s
subject: %s
from: %s
to: %s
cc: %s
internal_date: %s
sent_date: %s
size_bytes: %d
attachment_count: %d
%s
Message text:
%s`, msg.ID, msg.Mailbox, msg.Folder, subject, from, strings.Join(msg.To, ", "),
		strings.Join(msg.Cc, ", "), msg.InternalDate.Format("2006-01-02T15:04:05Z07:00"),
		sent, msg.Size, msg.AttachmentCount, attachmentLines(msg.Attachments), text)
}

// Bounds on the attachment list in a prompt. A message can list up to the
// ingest part limit (200); names beyond these add tokens, not judgement.
const (
	maxPromptAttachments    = 20
	maxAttachmentNameLength = 120
)

// attachmentLines renders the attachment list for the prompt, one per line,
// or nothing when there is none (OPS-009). Names and types are the sender's
// text, so control characters are flattened: a name holding a newline cannot
// start a line of its own and pass for part of the prompt's structure.
func attachmentLines(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("attachments:\n")
	for i, a := range atts {
		if i == maxPromptAttachments {
			fmt.Fprintf(&b, "- ... and %d more\n", len(atts)-maxPromptAttachments)
			break
		}
		name := "(no name)"
		if a.Filename != nil && strings.TrimSpace(*a.Filename) != "" {
			name = flattenControl(*a.Filename)
			if utf8.RuneCountInString(name) > maxAttachmentNameLength {
				name = string([]rune(name)[:maxAttachmentNameLength]) + "..."
			}
		}
		fmt.Fprintf(&b, "- %s (%s, %d bytes)\n", name, flattenControl(a.ContentType), a.SizeBytes)
	}
	return b.String()
}

// flattenControl replaces every control character with a space.
func flattenControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// truncationMarker is appended to any summary this worker shortened, so a
// reader can tell a terse model answer from a clipped one.
const truncationMarker = "\n\n[truncated]"

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(max + 64)
	count := 0
	for _, r := range s {
		if count >= max {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String() + truncationMarker
}

// fallbackCategory is the category used when the model's answer is outside the
// configured vocabulary (RA6X-044).
//
// It used to be the literal "other" whatever the operator had configured, so a
// deployment whose categories were, say, ["work", "personal"] emitted a
// category no consumer of that vocabulary understands and no filter would ever
// select — the advisory label silently stopped meaning anything. The fallback
// now comes from the configured set: "other" when it is present, since that is
// the conventional bucket and the shipped default includes it, and otherwise
// the first configured category, which is a choice the operator made.
func fallbackCategory(cfg *Config) string {
	for _, c := range cfg.Worker.Categories {
		if normalizeLabel(c) == "other" {
			return "other"
		}
	}
	for _, c := range cfg.Worker.Categories {
		if n := normalizeLabel(c); n != "" {
			return n
		}
	}
	// Validate refuses an empty vocabulary, so this is unreachable in a loaded
	// config; a hand-built one in a test gets the conventional bucket.
	return "other"
}

func normalizeAnnotation(cfg *Config, ann LLMAnnotation) AnnotationPut {
	summary := strings.TrimSpace(ann.Summary)
	if summary == "" {
		summary = "(no useful summary produced)"
	}
	summary = truncateRunes(summary, cfg.Worker.MaxSummaryChars)

	category := normalizeLabel(ann.Category)
	if !containsLabel(cfg.Worker.Categories, category) {
		category = fallbackCategory(cfg)
	}
	// Clamp to the contract's category limit. normalizeLabel emits ASCII only,
	// so a byte slice cannot split a multibyte rune (RA6X-044).
	if len(category) > apiMaxCategoryBytes {
		category = category[:apiMaxCategoryBytes]
	}

	allowed := map[string]bool{}
	for _, tag := range cfg.Worker.AllowedTags {
		allowed[normalizeLabel(tag)] = true
	}
	// Effective tag cap: honor the operator's MaxTags but never exceed the
	// epistula-api contract's per-PUT tag count, so a misconfigured (or
	// Validate-bypassed) MaxTags can't produce a 422-rejected PUT (R-033).
	effMaxTags := cfg.Worker.MaxTags
	if effMaxTags > apiMaxTagCount {
		effMaxTags = apiMaxTagCount
	}
	tags := make([]string, 0, min(len(ann.Tags), effMaxTags))
	seen := map[string]bool{}
	for _, tag := range ann.Tags {
		tag = normalizeLabel(tag)
		// Clamp to the contract's per-tag byte limit. normalizeLabel emits
		// ASCII only, so a byte slice can't split a multibyte rune (R-033).
		if len(tag) > apiMaxTagBytes {
			tag = tag[:apiMaxTagBytes]
		}
		if tag == "" || seen[tag] {
			continue
		}
		if len(allowed) > 0 && !allowed[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
		if len(tags) >= effMaxTags {
			break
		}
	}
	// The empty-tags fallback must respect the SAME policy every other tag
	// went through (RA6X-044). Appending the category unconditionally bypassed
	// the allowed-tags set and the per-tag byte limit, so a deployment with a
	// restrictive allowed_tags list got a tag it had explicitly excluded, and a
	// long category produced a tag epistula-api rejects 422 — re-annotating that
	// message on every pass, forever.
	//
	// When the category is not an acceptable tag, the annotation goes out with
	// NO tags. epistula-api accepts an empty array, and "this model had no tag to
	// give" is the honest record; inventing one that violates the operator's
	// vocabulary is not.
	if len(tags) == 0 {
		if candidate := category; candidate != "" && len(candidate) <= apiMaxTagBytes {
			if len(allowed) == 0 || allowed[candidate] {
				tags = append(tags, candidate)
			}
		}
	}

	put := AnnotationPut{
		Model:    cfg.Worker.AnnotationModel,
		Tags:     tags,
		Category: &category,
		Summary:  &summary,
	}
	// Finally, the SERIALIZED body has to fit epistula-api's cap. MaxSummaryChars
	// counts characters; the PUT carries JSON, where one character can become
	// six bytes (a multibyte rune escaped, or an escaped control), so a
	// configured limit that looks modest can still produce an over-cap body.
	// Shrinking the summary is the right lever: it is the only unbounded field
	// and the only one whose loss is a shorter description rather than a
	// missing label.
	shrinkToAPIBody(&put)
	return put
}

// shrinkToAPIBody trims put.Summary until the JSON body fits epistula-api's
// annotation cap. Halving is enough: the other fields are bounded by their own
// limits and are far smaller than the cap, so the loop terminates quickly and
// always leaves a valid, if shorter, summary (RA6X-044).
func shrinkToAPIBody(put *AnnotationPut) {
	for put.Summary != nil && *put.Summary != "" {
		body, err := json.Marshal(put)
		if err != nil || len(body) <= apiMaxAnnotationBytes {
			return
		}
		// Halve the CONTENT, not the rendered string: strip any marker a
		// previous round appended so it is not carried forward or duplicated.
		content := strings.TrimSuffix(*put.Summary, truncationMarker)
		runes := []rune(content)
		if len(runes) < 2 {
			// Cannot get meaningfully shorter; drop the summary rather than
			// emit a body the API will refuse.
			put.Summary = nil
			return
		}
		next := string(runes[:len(runes)/2]) + truncationMarker
		put.Summary = &next
	}
}

func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	out := strings.Builder{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		}
	}
	return strings.Trim(out.String(), "-_")
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if normalizeLabel(label) == want {
			return true
		}
	}
	return false
}
