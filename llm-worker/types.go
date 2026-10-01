package main

import "time"

type Annotation struct {
	Model     string    `json:"model"`
	Tags      []string  `json:"tags"`
	Category  *string   `json:"category,omitempty"`
	Summary   *string   `json:"summary,omitempty"`
	TokensIn  *int64    `json:"tokens_in,omitempty"`
	TokensOut *int64    `json:"tokens_out,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type Message struct {
	ID              int64        `json:"id"`
	UID             int64        `json:"uid"`
	Mailbox         string       `json:"mailbox,omitempty"`
	Folder          string       `json:"folder,omitempty"`
	InternalDate    time.Time    `json:"internal_date"`
	SentDate        *time.Time   `json:"sent_date,omitempty"`
	Subject         *string      `json:"subject,omitempty"`
	From            *string      `json:"from,omitempty"`
	To              []string     `json:"to,omitempty"`
	Cc              []string     `json:"cc,omitempty"`
	MessageID       *string      `json:"message_id,omitempty"`
	InReplyTo       *string      `json:"in_reply_to,omitempty"`
	Flags           []string     `json:"flags"`
	Size            int64        `json:"size"`
	AttachmentCount int64        `json:"attachment_count"`
	Attachments     []Attachment `json:"attachments,omitempty"`
	TextBody        *string      `json:"text_body,omitempty"`
	Annotations     []Annotation `json:"annotations,omitempty"`
}

// Attachment is one attachment's metadata as /v1/export lists it (OPS-009).
// The worker reads only what the prompt shows: the name, the type and the
// size. Both strings come from the sender.
type Attachment struct {
	Filename    *string `json:"filename,omitempty"`
	ContentType string  `json:"content_type"`
	SizeBytes   int64   `json:"size_bytes"`
}

type LLMAnnotation struct {
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags"`
	Category string   `json:"category"`
	// Set only by a combined annotate-and-classify answer, and only after
	// checkArchiveFields has validated them; never decoded directly.
	ArchiveCategory   string   `json:"-"`
	ArchiveConfidence *float64 `json:"-"`
}

type AnnotationPut struct {
	Model     string   `json:"model"`
	Tags      []string `json:"tags"`
	Category  *string  `json:"category"`
	Summary   *string  `json:"summary"`
	TokensIn  *int64   `json:"tokens_in,omitempty"`
	TokensOut *int64   `json:"tokens_out,omitempty"`
}
