package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/recipients"
)

// POST /v1/mailboxes/{mailbox}/submissions — hand one complete RFC 5322
// message to the MTA's authenticated submission service as that mailbox.
//
// The message is relayed byte for byte. Its line endings are made CRLF, as
// SMTP requires, and nothing else is changed: no header is added, removed,
// folded or re-encoded, so signatures computed over the message (DKIM,
// S/MIME, OpenPGP) still verify after it is sent. A message a server would
// have to rewrite (a bare CR, a NUL, a line over 998 octets) is refused
// rather than altered.
//
// epistula-api is the token's policy point, not the outbound one. It checks
// submit_message and the mailbox scope, and refuses an envelope sender, From
// or Sender address that is not one of the mailbox's own aliases. The MTA
// then authenticates the configured account, checks the envelope sender
// against that login again, signs, queues and delivers. epistula-api never
// queues, retries, or stores the message, and writes nothing to the database.

// maxSubmissionMessageBytes caps submission.max_message_bytes at the LDA's own
// ceiling, past which no Epistula component handles a message.
const maxSubmissionMessageBytes = 50 << 20

// maxLineOctets is RFC 5322 section 2.1.1's limit on one line, excluding CRLF.
const maxLineOctets = 998

// maxSMTPReply bounds an SMTP reply quoted back to a client or a log.
const maxSMTPReply = 512

// singleFields are the RFC 5322 section 3.6 fields a message may carry at
// most once. Two From fields are the classic way to show a reader one author
// while a signature covers another, so a duplicate is refused rather than
// relayed. Keys are in textproto's canonical form, as net/mail stores them.
var singleFields = []string{
	"Date", "From", "Sender", "Reply-To", "To", "Cc", "Bcc",
	"Message-Id", "In-Reply-To", "References", "Subject",
}

type submissionRequest struct {
	// EnvelopeFrom is the SMTP reverse path. Empty uses the Sender field when
	// present, otherwise the single From address.
	EnvelopeFrom string `json:"envelope_from"`
	// Recipients are the SMTP forward paths. Empty uses every To and Cc
	// address; blind recipients must be listed here, because the message
	// itself may not carry a Bcc field.
	Recipients []string `json:"recipients"`
	// MessageBase64 is the complete message, base64-encoded so any octets
	// survive JSON.
	MessageBase64 []byte `json:"message_base64"`
}

type submissionResponse struct {
	QueueID      string   `json:"queue_id,omitempty"`
	SMTPReply    string   `json:"smtp_reply"`
	Mailbox      string   `json:"mailbox"`
	EnvelopeFrom string   `json:"envelope_from"`
	Recipients   []string `json:"recipients"`
	MessageID    string   `json:"message_id,omitempty"`
	SizeBytes    int      `json:"size_bytes"`
	// SHA256 is the digest of the bytes handed to the server: the received
	// message with CRLF line endings, before SMTP dot-stuffing.
	SHA256 string `json:"sha256"`
	// LineEndingsNormalized is true when that differs from what was received
	// (LF line endings, or no line ending after the last line).
	LineEndingsNormalized bool `json:"line_endings_normalized"`
}

// outgoingMessage is a validated submission, ready for the relay.
type outgoingMessage struct {
	data         []byte // canonical CRLF bytes, exactly as handed to the server
	normalized   bool   // data differs from the bytes received
	envelopeFrom string
	recipients   []string
	authors      []string // the From and Sender addresses
	messageID    string
	eightBit     bool // an octet above 0x7F anywhere: needs 8BITMIME
	needsUTF8    bool // a non-ASCII header or envelope address: needs SMTPUTF8
}

// submissionBodyLimit bounds the JSON request body: the base64 expansion of
// the largest accepted message plus room for the envelope fields.
func submissionBodyLimit(maxMessageBytes int) int64 {
	return int64(maxMessageBytes+2)/3*4 + 64<<10
}

// canonicalMessage returns raw with every line ending as CRLF and reports
// whether that changed anything. A message that does not end in a line ending
// gets one, as SMTP DATA would add it anyway. A NUL, a carriage return that
// does not end a line, or a line over 998 octets is refused: a server would
// have to rewrite each of them, and the rewritten message would no longer
// match a signature computed over the original.
func canonicalMessage(raw []byte) ([]byte, bool, error) {
	out := make([]byte, 0, len(raw)+len(raw)/32+2)
	changed := false
	line, lineLen := 1, 0
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case 0:
			return nil, false, fmt.Errorf("line %d contains a NUL octet", line)
		case '\r':
			if i+1 == len(raw) || raw[i+1] != '\n' {
				return nil, false, fmt.Errorf("line %d contains a carriage return that does not end the line", line)
			}
			out = append(out, '\r', '\n')
			i++
			line, lineLen = line+1, 0
		case '\n':
			out = append(out, '\r', '\n')
			changed = true
			line, lineLen = line+1, 0
		default:
			lineLen++
			if lineLen > maxLineOctets {
				return nil, false, fmt.Errorf("line %d is longer than %d octets (RFC 5322 section 2.1.1); a server would have to split it", line, maxLineOctets)
			}
			out = append(out, c)
		}
	}
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\r\n")) {
		out = append(out, '\r', '\n')
		changed = true
	}
	return out, changed, nil
}

// envelopeAddress checks an address for use as an SMTP path: an addr-spec
// with nothing a MAIL or RCPT command line could misread.
func envelopeAddress(addr string) error {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return fmt.Errorf("%q is not an address of the form name@domain", addr)
	}
	for _, r := range addr {
		if r <= ' ' || r == 0x7f || r == '<' || r == '>' {
			return fmt.Errorf("%q cannot be used as an SMTP address", addr)
		}
	}
	return nil
}

// bareAddress parses a request field that must be one bare addr-spec.
func bareAddress(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("%s: %q is not an email address", field, s)
	}
	if a.Name != "" || a.Address != s {
		return "", fmt.Errorf("%s: %q must be a bare address such as name@domain, without a display name or angle brackets", field, s)
	}
	if err := envelopeAddress(a.Address); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return a.Address, nil
}

// headerAddresses returns the addresses of an address-list field, or none
// when the field is absent.
func headerAddresses(h mail.Header, field string) ([]string, error) {
	if len(h[field]) == 0 {
		return nil, nil
	}
	list, err := h.AddressList(field)
	if err != nil {
		return nil, fmt.Errorf("the %s field does not parse: %v", field, err)
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if err := envelopeAddress(a.Address); err != nil {
			return nil, fmt.Errorf("the %s field: %w", field, err)
		}
		out = append(out, a.Address)
	}
	return out, nil
}

func hasNonASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

// prepareSubmission validates a request against the message rules and the
// configured limits. Every error is the client's to correct (422).
func prepareSubmission(req submissionRequest, cfg SubmissionConfig) (*outgoingMessage, error) {
	if len(req.MessageBase64) == 0 {
		return nil, errors.New("message_base64 is required: the complete message, headers and body, base64-encoded")
	}
	data, normalized, err := canonicalMessage(req.MessageBase64)
	if err != nil {
		return nil, fmt.Errorf("the message cannot be sent unchanged: %w", err)
	}
	if len(data) > cfg.MaxMessageBytes {
		return nil, fmt.Errorf("the message is %d bytes with CRLF line endings, over this server's limit of %d bytes", len(data), cfg.MaxMessageBytes)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the message header does not parse: %v", err)
	}
	h := msg.Header
	for _, k := range singleFields {
		if n := len(h[k]); n > 1 {
			return nil, fmt.Errorf("the message has %d %s fields; RFC 5322 allows one", n, k)
		}
	}
	if len(h["Bcc"]) != 0 {
		return nil, errors.New("the message has a Bcc field, which every recipient would receive; remove it and list blind recipients in recipients instead")
	}
	if _, err := h.Date(); err != nil {
		return nil, fmt.Errorf("the message needs a valid Date field: %v", err)
	}
	from, err := headerAddresses(h, "From")
	if err != nil {
		return nil, err
	}
	if len(from) == 0 {
		return nil, errors.New("the message needs a From field naming its author")
	}
	sender, err := headerAddresses(h, "Sender")
	if err != nil {
		return nil, err
	}
	switch {
	case len(h["Sender"]) != 0 && len(sender) != 1:
		return nil, errors.New("the Sender field must name exactly one address")
	case len(sender) == 0 && len(from) > 1:
		return nil, fmt.Errorf("the From field names %d authors; RFC 5322 then requires a Sender field naming the one who sends", len(from))
	}

	out := &outgoingMessage{
		data:       data,
		normalized: normalized,
		authors:    append(append([]string{}, from...), sender...),
		messageID:  strings.TrimSpace(h.Get("Message-Id")),
		eightBit:   hasNonASCII(data),
	}
	if end := bytes.Index(data, []byte("\r\n\r\n")); end >= 0 {
		out.needsUTF8 = hasNonASCII(data[:end])
	} else {
		out.needsUTF8 = out.eightBit
	}

	switch {
	case strings.TrimSpace(req.EnvelopeFrom) != "":
		if out.envelopeFrom, err = bareAddress("envelope_from", req.EnvelopeFrom); err != nil {
			return nil, err
		}
	case len(sender) == 1:
		out.envelopeFrom = sender[0]
	default:
		out.envelopeFrom = from[0]
	}

	rcpts := req.Recipients
	if len(rcpts) == 0 {
		to, err := headerAddresses(h, "To")
		if err != nil {
			return nil, err
		}
		cc, err := headerAddresses(h, "Cc")
		if err != nil {
			return nil, err
		}
		rcpts = append(to, cc...)
		if len(rcpts) == 0 {
			return nil, errors.New("no recipients: list them in recipients, or address the message with To or Cc")
		}
	} else {
		for i, r := range rcpts {
			if rcpts[i], err = bareAddress(fmt.Sprintf("recipients[%d]", i), r); err != nil {
				return nil, err
			}
		}
	}
	seen := make(map[string]bool, len(rcpts))
	for _, r := range rcpts {
		if key := strings.ToLower(r); !seen[key] {
			seen[key] = true
			out.recipients = append(out.recipients, r)
		}
	}
	if len(out.recipients) > cfg.MaxRecipients {
		return nil, fmt.Errorf("%d recipients is over this server's limit of %d per message", len(out.recipients), cfg.MaxRecipients)
	}
	for _, a := range append([]string{out.envelopeFrom}, out.recipients...) {
		if hasNonASCII([]byte(a)) {
			out.needsUTF8 = true
		}
	}
	return out, nil
}

// ---- the relay ----

// submissionKind classifies how a session ended, for the HTTP status, the
// metric and the log.
type submissionKind string

const (
	// submissionUnreachable: the message was not sent; the server could not
	// be reached, refused TLS, or broke off before the end of the message.
	submissionUnreachable submissionKind = "failed"
	// submissionCredential: the server refused the configured account.
	submissionCredential submissionKind = "credential"
	// submissionRejected: a permanent refusal (5xx), or a message this
	// server cannot carry. Not sent; resending it unchanged fails again.
	submissionRejected submissionKind = "rejected"
	// submissionDeferred: a temporary refusal (4xx). Not sent.
	submissionDeferred submissionKind = "deferred"
	// submissionUnknown: the whole message was written but no final reply
	// was read, so the server may have queued it.
	submissionUnknown submissionKind = "unknown"
)

type recipientRejection struct {
	Recipient string
	Code      int
	Reply     string
}

// submissionError describes a session that did not end with the message
// queued. reason is safe to show the client; err is for the log only.
type submissionError struct {
	kind     submissionKind
	stage    string
	code     int
	reply    string
	reason   string
	rejected []recipientRejection
	err      error
}

func (e *submissionError) Error() string {
	if e.err != nil {
		return e.stage + ": " + e.err.Error()
	}
	return e.stage + ": " + e.reason
}

func (e *submissionError) Unwrap() error { return e.err }

func boundReply(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxSMTPReply {
		return s[:maxSMTPReply] + "…"
	}
	return s
}

// smtpFailure classifies an error from one stage of the session.
func smtpFailure(stage, reason string, err error) *submissionError {
	e := &submissionError{kind: submissionUnreachable, stage: stage, reason: reason, err: err}
	var te *textproto.Error
	if errors.As(err, &te) {
		e.code, e.reply = te.Code, boundReply(te.Msg)
		switch {
		case te.Code >= 400 && te.Code < 500:
			e.kind = submissionDeferred
		case te.Code >= 500 && stage == "auth":
			e.kind = submissionCredential
		case te.Code >= 500 && (stage == "envelope" || stage == "data" || stage == "result"):
			e.kind = submissionRejected
		}
	}
	return e
}

type submissionReceipt struct {
	reply   string
	queueID string
}

// queueIDRe finds the queue id in a Postfix-style final reply
// ("250 2.0.0 Ok: queued as 4F2D61A3B7").
var queueIDRe = regexp.MustCompile(`(?i)\bqueued as ([A-Za-z0-9]+)`)

// submissionRelay speaks RFC 6409 submission to one MTA endpoint.
type submissionRelay struct {
	address    string
	serverName string
	heloName   string
	timeout    time.Duration
	// rootCAs verifies the server certificate; nil uses the system roots.
	rootCAs *x509.CertPool
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
}

var heloNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func newSubmissionRelay(cfg SubmissionConfig) *submissionRelay {
	helo := "localhost"
	if h, err := os.Hostname(); err == nil && heloNameRe.MatchString(h) {
		helo = h
	}
	dialer := &net.Dialer{Timeout: cfg.TimeoutDuration()}
	return &submissionRelay{
		address:    cfg.Address,
		serverName: cfg.ServerName(),
		heloName:   helo,
		timeout:    cfg.TimeoutDuration(),
		dial:       dialer.DialContext,
	}
}

// submit runs one SMTP session: STARTTLS (required), AUTH PLAIN, MAIL, every
// RCPT, DATA. Every recipient must be accepted before DATA is sent: when any
// is refused the transaction is reset and nothing is sent, so a message never
// reaches only some of the recipients the caller named. It never retries.
func (rl *submissionRelay) submit(ctx context.Context, acct SubmissionAccount, msg *outgoingMessage) (submissionReceipt, *submissionError) {
	conn, err := rl.dial(ctx, "tcp", rl.address)
	if err != nil {
		return submissionReceipt{}, smtpFailure("connect", "could not reach the submission service", err)
	}
	deadline := time.Now().Add(rl.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return submissionReceipt{}, smtpFailure("connect", "could not reach the submission service", err)
	}
	// Cancellation expires the deadline, so a blocked read or write returns
	// at once instead of holding the session open until the timeout.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, rl.serverName)
	if err != nil {
		conn.Close()
		return submissionReceipt{}, smtpFailure("connect", "the submission service did not greet", err)
	}
	defer c.Close()
	if err := c.Hello(rl.heloName); err != nil {
		return submissionReceipt{}, smtpFailure("connect", "the submission service refused EHLO", err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return submissionReceipt{}, &submissionError{kind: submissionUnreachable, stage: "starttls",
			reason: "the submission service does not offer STARTTLS; the account password is never sent without TLS"}
	}
	if err := c.StartTLS(&tls.Config{ServerName: rl.serverName, MinVersion: tls.VersionTLS12, RootCAs: rl.rootCAs}); err != nil {
		return submissionReceipt{}, smtpFailure("starttls", "TLS with the submission service failed", err)
	}
	if ok, mechs := c.Extension("AUTH"); !ok || !containsFold(strings.Fields(mechs), "PLAIN") {
		return submissionReceipt{}, &submissionError{kind: submissionUnreachable, stage: "auth",
			reason: "the submission service does not offer AUTH PLAIN after STARTTLS"}
	}
	if err := c.Auth(smtp.PlainAuth("", acct.Username, acct.Password, rl.serverName)); err != nil {
		return submissionReceipt{}, smtpFailure("auth", "the submission service refused the configured account", err)
	}

	// Refuse what this server cannot carry before any transaction starts.
	if ok, p := c.Extension("SIZE"); ok {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil && n > 0 && int64(len(msg.data)) > n {
			return submissionReceipt{}, &submissionError{kind: submissionRejected, stage: "envelope",
				reason: fmt.Sprintf("the submission service accepts messages up to %d bytes; this one is %d", n, len(msg.data))}
		}
	}
	if ok, _ := c.Extension("8BITMIME"); msg.eightBit && !ok {
		return submissionReceipt{}, &submissionError{kind: submissionRejected, stage: "envelope",
			reason: "the message contains 8-bit octets and the submission service does not offer 8BITMIME"}
	}
	if ok, _ := c.Extension("SMTPUTF8"); msg.needsUTF8 && !ok {
		return submissionReceipt{}, &submissionError{kind: submissionRejected, stage: "envelope",
			reason: "the message has non-ASCII header or address text and the submission service does not offer SMTPUTF8"}
	}

	if err := c.Mail(msg.envelopeFrom); err != nil {
		return submissionReceipt{}, smtpFailure("envelope", "the submission service refused the envelope sender", err)
	}
	var rejected []recipientRejection
	for _, rcpt := range msg.recipients {
		if err := c.Rcpt(rcpt); err != nil {
			var te *textproto.Error
			if !errors.As(err, &te) {
				return submissionReceipt{}, smtpFailure("envelope", "the submission service broke off during the recipients", err)
			}
			rejected = append(rejected, recipientRejection{Recipient: rcpt, Code: te.Code, Reply: boundReply(te.Msg)})
		}
	}
	if len(rejected) > 0 {
		_ = c.Reset()
		_ = c.Quit()
		e := &submissionError{kind: submissionDeferred, stage: "envelope", rejected: rejected,
			reason: "the submission service refused recipients"}
		for _, r := range rejected {
			if r.Code >= 500 {
				e.kind = submissionRejected
			}
		}
		e.code, e.reply = rejected[0].Code, rejected[0].Reply
		return submissionReceipt{}, e
	}

	// DATA through c.Text rather than c.Data, whose Close discards the final
	// reply: that reply carries the queue id the caller needs to trace the
	// message in the mail log.
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return submissionReceipt{}, smtpFailure("data", "the submission service broke off before the message", err)
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(354)
	c.Text.EndResponse(id)
	if err != nil {
		return submissionReceipt{}, smtpFailure("data", "the submission service refused DATA", err)
	}
	dw := c.Text.DotWriter()
	if _, err := dw.Write(msg.data); err != nil {
		// The terminating dot was never written, so the server discards
		// the partial message.
		return submissionReceipt{}, smtpFailure("data", "the connection failed while the message was being sent", err)
	}
	if err := dw.Close(); err != nil {
		return submissionReceipt{}, &submissionError{kind: submissionUnknown, stage: "result", err: err,
			reason: "the connection failed while the end of the message was being sent"}
	}
	code, reply, err := c.Text.ReadResponse(250)
	if err != nil {
		var te *textproto.Error
		if errors.As(err, &te) {
			return submissionReceipt{}, smtpFailure("result", "the submission service refused the message", err)
		}
		return submissionReceipt{}, &submissionError{kind: submissionUnknown, stage: "result", err: err,
			reason: "the whole message was sent but the submission service's reply was not received"}
	}
	// The message is queued; a failed QUIT changes nothing.
	_ = c.Quit()
	r := submissionReceipt{reply: boundReply(strconv.Itoa(code) + " " + reply)}
	if m := queueIDRe.FindStringSubmatch(reply); m != nil {
		r.queueID = m[1]
	}
	return r, nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// ---- the handler ----

func (s *server) handleSubmission(w http.ResponseWriter, r *http.Request, tok *apiToken) {
	if !requirePermission(w, r, tok, auth.PermissionSubmitMessage) {
		return
	}
	mailbox := r.PathValue("mailbox")
	mailboxID, ok := s.lookupScopedMailbox(w, r, tok, mailbox)
	if !ok {
		return
	}
	if !requireMailboxScope(w, r, tok, mailboxID, mailbox) {
		return
	}
	acct, configured := s.cfg.Submission.Account(mailbox)
	if s.relay == nil || !configured {
		problemUnavailable(w, r, "Message submission is not configured for mailbox '"+mailbox+"'.")
		return
	}

	var req submissionRequest
	if !decodeJSONBody(w, r, &req, submissionBodyLimit(s.cfg.Submission.MaxMessageBytes)) {
		return
	}
	out, err := prepareSubmission(req, s.cfg.Submission)
	if err != nil {
		problemUnprocessable(w, r, err.Error())
		return
	}
	if !s.checkSenders(w, r, mailboxID, mailbox, out) {
		return
	}
	if !s.submitLimiter.allow(tok.ID) {
		problemTooMany(w, r, fmt.Sprintf("This token has used its budget of %d messages per hour; nothing was sent.",
			s.cfg.Submission.MessagesPerHour))
		return
	}

	sum := sha256.Sum256(out.data)
	digest := hex.EncodeToString(sum[:])
	// A session that has started runs to its end even if the client goes
	// away: abandoning it after the final dot would leave even this server
	// unable to say whether the message was queued. It stays bounded by the
	// submission timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.Submission.TimeoutDuration())
	defer cancel()
	receipt, serr := s.relay.submit(ctx, acct, out)
	if serr != nil {
		metricSubmissions.WithLabelValues(string(serr.kind)).Inc()
		level := slog.LevelWarn
		if serr.kind == submissionCredential || serr.kind == submissionUnknown {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "message submission failed",
			"token", tok.Name, "mailbox", mailbox, "outcome", string(serr.kind), "stage", serr.stage,
			"smtp_code", serr.code, "recipients", len(out.recipients), "bytes", len(out.data),
			"sha256", digest[:16], "err", serr)
		writeSubmissionProblem(w, r, mailbox, serr)
		return
	}
	metricSubmissions.WithLabelValues("queued").Inc()
	slog.Info("message submitted",
		"token", tok.Name, "mailbox", mailbox, "queue_id", receipt.queueID,
		"recipients", len(out.recipients), "bytes", len(out.data), "sha256", digest[:16],
		"message_id", out.messageID)
	writeJSON(w, http.StatusAccepted, submissionResponse{
		QueueID:               receipt.queueID,
		SMTPReply:             receipt.reply,
		Mailbox:               mailbox,
		EnvelopeFrom:          out.envelopeFrom,
		Recipients:            out.recipients,
		MessageID:             out.messageID,
		SizeBytes:             len(out.data),
		SHA256:                digest,
		LineEndingsNormalized: out.normalized,
	})
}

// checkSenders refuses a disabled mailbox, and any envelope sender, From or
// Sender address that is not one of the mailbox's explicit aliases. A
// catchall alias authorizes nothing, matching what the submission service
// lets the account send as.
func (s *server) checkSenders(w http.ResponseWriter, r *http.Request, mailboxID int64, mailbox string, out *outgoingMessage) bool {
	var disabled bool
	err := s.pool.QueryRow(r.Context(), `SELECT disabled_at IS NOT NULL FROM mailboxes WHERE id = $1`, mailboxID).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		problemNotFound(w, r, "Mailbox '"+mailbox+"' does not exist.")
		return false
	}
	if err != nil {
		slog.Error("submission mailbox lookup", "mailbox_id", mailboxID, "err", err)
		problemInternal(w, r)
		return false
	}
	if disabled {
		problemForbidden(w, r, "Mailbox '"+mailbox+"' is disabled and cannot send.")
		return false
	}
	checked := map[string]bool{}
	for _, addr := range append([]string{out.envelopeFrom}, out.authors...) {
		local, domain, err := recipients.SplitAddress(addr)
		if err != nil {
			problemUnprocessable(w, r, fmt.Sprintf("%q is not an address this server can check: %v", addr, err))
			return false
		}
		key := local + "@" + domain
		if checked[key] {
			continue
		}
		checked[key] = true
		var owned bool
		if err := s.pool.QueryRow(r.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM aliases a JOIN domains d ON d.id = a.domain_id
				 WHERE a.mailbox_id = $1 AND a.localpart <> ''
				   AND a.localpart = $2 AND d.name = $3)`,
			mailboxID, local, domain).Scan(&owned); err != nil {
			slog.Error("submission sender lookup", "mailbox_id", mailboxID, "err", err)
			problemInternal(w, r)
			return false
		}
		if !owned {
			problemForbidden(w, r, fmt.Sprintf("%s is not an address of mailbox '%s': the envelope sender and every From and Sender address must be one of its own aliases (a catchall does not count).", addr, mailbox))
			return false
		}
	}
	return true
}

// writeSubmissionProblem says whether the message was sent, which is the one
// thing a caller deciding whether to send again needs to know.
func writeSubmissionProblem(w http.ResponseWriter, r *http.Request, mailbox string, e *submissionError) {
	reply := ""
	if e.code != 0 {
		reply = fmt.Sprintf(" (SMTP %d %s)", e.code, e.reply)
	}
	switch e.kind {
	case submissionRejected:
		if len(e.rejected) > 0 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "submission-rejected", "Submission Rejected",
				"Nothing was sent: the submission service refused "+describeRejections(e.rejected)+".")
			return
		}
		writeProblem(w, r, http.StatusUnprocessableEntity, "submission-rejected", "Submission Rejected",
			"Nothing was sent: "+e.reason+reply+".")
	case submissionDeferred:
		detail := e.reason + reply
		if len(e.rejected) > 0 {
			detail = "the submission service temporarily refused " + describeRejections(e.rejected)
		}
		writeProblem(w, r, http.StatusServiceUnavailable, "submission-deferred", "Submission Deferred",
			"Nothing was sent: "+detail+". The refusal is temporary; the message may be sent again later.")
	case submissionCredential:
		writeProblem(w, r, http.StatusBadGateway, "submission-failed", "Bad Gateway",
			"Nothing was sent: the submission service refused this server's account for mailbox '"+mailbox+"'"+reply+".")
	case submissionUnknown:
		writeProblem(w, r, http.StatusGatewayTimeout, "submission-outcome-unknown", "Submission Outcome Unknown",
			"The message may have been sent: "+e.reason+". Do not send it again until the mail log or a recipient shows it was not delivered.")
	default:
		writeProblem(w, r, http.StatusBadGateway, "submission-failed", "Bad Gateway",
			"Nothing was sent: "+e.reason+reply+".")
	}
}

// describeRejections names the refused recipients, the first few in full.
func describeRejections(rs []recipientRejection) string {
	const shown = 3
	parts := make([]string, 0, shown+1)
	for i, r := range rs {
		if i == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(rs)-shown))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (SMTP %d %s)", r.Recipient, r.Code, r.Reply))
	}
	if len(rs) == 1 {
		return "recipient " + parts[0]
	}
	return "recipients " + strings.Join(parts, ", ")
}
