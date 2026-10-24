package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// signedFixture is a message as a signer would have produced it, with LF
// line endings and a body line that begins with a dot.
const signedFixture = "From: Alice <alice@mail.test.invalid>\n" +
	"To: Carol <carol@elsewhere.invalid>, dave@elsewhere.invalid\n" +
	"Cc: Erin <erin@elsewhere.invalid>, carol@elsewhere.invalid\n" +
	"Subject: signed\n" +
	"Date: Sat, 24 Oct 2026 12:00:00 +0000\n" +
	"Message-ID: <m1@mail.test.invalid>\n" +
	"DKIM-Signature: v=1; a=rsa-sha256; d=mail.test.invalid; s=sel;\n" +
	"\th=from:to:subject:date; bh=abc=; b=def=\n" +
	"MIME-Version: 1.0\n" +
	"Content-Type: text/plain; charset=utf-8\n" +
	"\n" +
	"First line\n" +
	".a line that starts with a dot\n" +
	"..and one with two\n"

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func testSubmissionConfig() SubmissionConfig {
	cfg := DefaultConfig().Submission
	cfg.Address = fakeSMTPName + ":587"
	cfg.Accounts = []SubmissionAccount{{Mailbox: "alice", Password: "test-only-password"}}
	return cfg
}

func TestCanonicalMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		changed bool
		err     string
	}{
		{name: "CRLF unchanged", in: "A: b\r\n\r\nbody\r\n", want: "A: b\r\n\r\nbody\r\n"},
		{name: "LF becomes CRLF", in: "A: b\n\nbody\n", want: "A: b\r\n\r\nbody\r\n", changed: true},
		{name: "mixed", in: "A: b\r\n\nbody\n", want: "A: b\r\n\r\nbody\r\n", changed: true},
		{name: "final line ending added", in: "A: b\r\n\r\nbody", want: "A: b\r\n\r\nbody\r\n", changed: true},
		{name: "998 octets", in: "A: b\r\n\r\n" + strings.Repeat("x", 998) + "\r\n", want: "A: b\r\n\r\n" + strings.Repeat("x", 998) + "\r\n"},
		{name: "999 octets", in: "A: b\r\n\r\n" + strings.Repeat("x", 999) + "\r\n", err: "line 3 is longer than 998"},
		{name: "bare CR", in: "A: b\r\n\r\nbo\rdy\r\n", err: "line 3 contains a carriage return"},
		{name: "trailing CR", in: "A: b\r\n\r\nbody\r", err: "carriage return"},
		{name: "NUL", in: "A: b\r\n\r\nbo\x00dy\r\n", err: "line 3 contains a NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := canonicalMessage([]byte(tc.in))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || changed != tc.changed {
				t.Fatalf("got %q changed=%v, want %q changed=%v", got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestPrepareSubmissionDerivesEnvelope(t *testing.T) {
	out, err := prepareSubmission(submissionRequest{MessageBase64: []byte(signedFixture)}, testSubmissionConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.data, crlf(signedFixture)) || !out.normalized {
		t.Errorf("data = %q normalized=%v", out.data, out.normalized)
	}
	if out.envelopeFrom != "alice@mail.test.invalid" {
		t.Errorf("envelope from = %q", out.envelopeFrom)
	}
	// To then Cc, with the second carol dropped.
	want := []string{"carol@elsewhere.invalid", "dave@elsewhere.invalid", "erin@elsewhere.invalid"}
	if strings.Join(out.recipients, ",") != strings.Join(want, ",") {
		t.Errorf("recipients = %v, want %v", out.recipients, want)
	}
	if strings.Join(out.authors, ",") != "alice@mail.test.invalid" {
		t.Errorf("authors = %v", out.authors)
	}
	if out.messageID != "<m1@mail.test.invalid>" || out.eightBit || out.needsUTF8 {
		t.Errorf("message id %q eightBit=%v needsUTF8=%v", out.messageID, out.eightBit, out.needsUTF8)
	}
}

func TestPrepareSubmissionExplicitEnvelope(t *testing.T) {
	msg := "From: Alice <alice@mail.test.invalid>\r\nSender: ops@mail.test.invalid\r\n" +
		"Date: Sat, 24 Oct 2026 12:00:00 +0000\r\nSubject: x\r\n\r\nbody\r\n"
	out, err := prepareSubmission(submissionRequest{
		Recipients:    []string{"Blind@Elsewhere.invalid", "blind@elsewhere.invalid", "other@elsewhere.invalid"},
		MessageBase64: []byte(msg),
	}, testSubmissionConfig())
	if err != nil {
		t.Fatal(err)
	}
	if out.normalized {
		t.Error("a CRLF message was reported as normalized")
	}
	if out.envelopeFrom != "ops@mail.test.invalid" {
		t.Errorf("envelope from = %q, want the Sender", out.envelopeFrom)
	}
	if strings.Join(out.recipients, ",") != "Blind@Elsewhere.invalid,other@elsewhere.invalid" {
		t.Errorf("recipients = %v", out.recipients)
	}
	if strings.Join(out.authors, ",") != "alice@mail.test.invalid,ops@mail.test.invalid" {
		t.Errorf("authors = %v", out.authors)
	}

	out, err = prepareSubmission(submissionRequest{EnvelopeFrom: "bounces@mail.test.invalid",
		Recipients: []string{"x@elsewhere.invalid"}, MessageBase64: []byte(msg)}, testSubmissionConfig())
	if err != nil {
		t.Fatal(err)
	}
	if out.envelopeFrom != "bounces@mail.test.invalid" {
		t.Errorf("explicit envelope from = %q", out.envelopeFrom)
	}
}

func TestPrepareSubmissionNonASCII(t *testing.T) {
	cfg := testSubmissionConfig()
	body := "From: alice@mail.test.invalid\r\nTo: x@elsewhere.invalid\r\nDate: Sat, 24 Oct 2026 12:00:00 +0000\r\n\r\ncafé\r\n"
	out, err := prepareSubmission(submissionRequest{MessageBase64: []byte(body)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.eightBit || out.needsUTF8 {
		t.Errorf("8-bit body: eightBit=%v needsUTF8=%v", out.eightBit, out.needsUTF8)
	}
	header := "From: alice@mail.test.invalid\r\nTo: x@elsewhere.invalid\r\nSubject: café\r\nDate: Sat, 24 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n"
	if out, err = prepareSubmission(submissionRequest{MessageBase64: []byte(header)}, cfg); err != nil {
		t.Fatal(err)
	}
	if !out.needsUTF8 {
		t.Error("a UTF-8 header did not require SMTPUTF8")
	}
}

func TestPrepareSubmissionRefusals(t *testing.T) {
	const date = "Date: Sat, 24 Oct 2026 12:00:00 +0000\r\n"
	const from = "From: alice@mail.test.invalid\r\n"
	const to = "To: x@elsewhere.invalid\r\n"
	cfg := testSubmissionConfig()
	cfg.MaxRecipients = 2
	cfg.MaxMessageBytes = 4096
	for _, tc := range []struct {
		name string
		req  submissionRequest
		want string
	}{
		{"empty", submissionRequest{}, "message_base64 is required"},
		{"too large", submissionRequest{MessageBase64: []byte(from + to + date + "\r\n" + strings.Repeat("x\r\n", 2000))}, "over this server's limit"},
		{"long line", submissionRequest{MessageBase64: []byte(from + to + date + "\r\n" + strings.Repeat("x", 1000) + "\r\n")}, "cannot be sent unchanged"},
		{"no From", submissionRequest{MessageBase64: []byte(to + date + "\r\nbody\r\n")}, "needs a From field"},
		{"no Date", submissionRequest{MessageBase64: []byte(from + to + "\r\nbody\r\n")}, "needs a valid Date"},
		{"two From", submissionRequest{MessageBase64: []byte(from + from + to + date + "\r\nbody\r\n")}, "2 From fields"},
		{"two Subject", submissionRequest{MessageBase64: []byte(from + to + date + "Subject: a\r\nSubject: b\r\n\r\nbody\r\n")}, "2 Subject fields"},
		{"Bcc", submissionRequest{MessageBase64: []byte(from + to + "Bcc: hidden@elsewhere.invalid\r\n" + date + "\r\nbody\r\n")}, "Bcc field"},
		{"two authors without Sender", submissionRequest{MessageBase64: []byte("From: a@mail.test.invalid, b@mail.test.invalid\r\n" + to + date + "\r\nbody\r\n")}, "requires a Sender"},
		{"no recipients", submissionRequest{MessageBase64: []byte(from + date + "\r\nbody\r\n")}, "no recipients"},
		{"too many recipients", submissionRequest{MessageBase64: []byte(from + "To: a@e.invalid, b@e.invalid, c@e.invalid\r\n" + date + "\r\nbody\r\n")}, "3 recipients"},
		{"display name recipient", submissionRequest{Recipients: []string{"Carol <carol@e.invalid>"}, MessageBase64: []byte(from + to + date + "\r\nbody\r\n")}, "must be a bare address"},
		{"not an address", submissionRequest{Recipients: []string{"carol"}, MessageBase64: []byte(from + to + date + "\r\nbody\r\n")}, "is not an email address"},
		{"bad envelope from", submissionRequest{EnvelopeFrom: "<a@mail.test.invalid>", MessageBase64: []byte(from + to + date + "\r\nbody\r\n")}, "envelope_from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareSubmission(tc.req, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSubmissionConfigValidation(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.Postgres.DSN = "postgres://u@127.0.0.1/db"
		c.Submission = testSubmissionConfig()
		return c
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if got := c.Submission.Accounts[0].Username; got != "alice" {
		t.Errorf("username defaulted to %q, want the mailbox name", got)
	}
	if got := c.Submission.ServerName(); got != fakeSMTPName {
		t.Errorf("server name = %q", got)
	}
	off := DefaultConfig()
	off.Postgres.DSN = "postgres://u@127.0.0.1/db"
	if err := off.Validate(); err != nil || off.Submission.Enabled() {
		t.Fatalf("default config: err=%v enabled=%v", err, off.Submission.Enabled())
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SubmissionConfig)
		want   string
	}{
		{"no address", func(s *SubmissionConfig) { s.Address = "" }, "host:port"},
		{"IP without server name", func(s *SubmissionConfig) { s.Address = "127.0.0.1:587" }, "tls_server_name"},
		{"bad timeout", func(s *SubmissionConfig) { s.Timeout = "soon" }, "submission.timeout"},
		{"no password", func(s *SubmissionConfig) { s.Accounts[0].Password = "" }, "has no password"},
		{"bad mailbox", func(s *SubmissionConfig) { s.Accounts[0].Mailbox = "Alice" }, "is not a mailbox name"},
		{"duplicate mailbox", func(s *SubmissionConfig) { s.Accounts = append(s.Accounts, s.Accounts[0]) }, "listed twice"},
		{"too large", func(s *SubmissionConfig) { s.MaxMessageBytes = maxSubmissionMessageBytes + 1 }, "must not exceed"},
		{"negative budget", func(s *SubmissionConfig) { s.MessagesPerHour = -1 }, "messages_per_hour"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c.Submission)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "test-only-password") {
				t.Fatal("a validation error echoed the password")
			}
		})
	}
	ip := base()
	ip.Submission.Address = "127.0.0.1:587"
	ip.Submission.TLSServerName = fakeSMTPName
	if err := ip.Validate(); err != nil {
		t.Fatalf("IP address with tls_server_name refused: %v", err)
	}
}

func preparedFixture(t *testing.T) *outgoingMessage {
	t.Helper()
	out, err := prepareSubmission(submissionRequest{MessageBase64: []byte(signedFixture)}, testSubmissionConfig())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var testAccount = SubmissionAccount{Mailbox: "alice", Username: "alice", Password: "test-only-password"}

func TestRelaySendsExactBytes(t *testing.T) {
	f := newFakeSMTP(t)
	msg := preparedFixture(t)
	receipt, serr := f.relay().submit(context.Background(), testAccount, msg)
	if serr != nil {
		t.Fatalf("submit: %v", serr)
	}
	if receipt.queueID != "4F2D61A3B7" || !strings.HasPrefix(receipt.reply, "250 2.0.0 Ok") {
		t.Errorf("receipt = %+v", receipt)
	}
	s := f.session(t)
	if !s.tls {
		t.Error("the session never started TLS")
	}
	if s.helo != "api.test.invalid" {
		t.Errorf("EHLO %q", s.helo)
	}
	if s.authUser != "alice" || s.authPass != "test-only-password" {
		t.Errorf("AUTH PLAIN as %q", s.authUser)
	}
	if !strings.HasPrefix(s.mail, "FROM:<alice@mail.test.invalid>") || !strings.Contains(s.mail, "BODY=8BITMIME") {
		t.Errorf("MAIL %q", s.mail)
	}
	if strings.Join(s.rcpts, ",") != strings.Join(msg.recipients, ",") {
		t.Errorf("RCPT %v, want %v", s.rcpts, msg.recipients)
	}
	if !bytes.Equal(s.data, crlf(signedFixture)) {
		t.Errorf("the server received\n%q\nwant\n%q", s.data, crlf(signedFixture))
	}
	if last := s.commands[len(s.commands)-1]; last != "QUIT" {
		t.Errorf("session ended with %s, want QUIT", last)
	}
}

func TestRelayRequiresSTARTTLS(t *testing.T) {
	f := newFakeSMTP(t, func(f *fakeSMTP) { f.noSTARTTLS = true })
	_, serr := f.relay().submit(context.Background(), testAccount, preparedFixture(t))
	if serr == nil || serr.kind != submissionUnreachable || serr.stage != "starttls" {
		t.Fatalf("err = %+v, want an unreachable starttls failure", serr)
	}
	s := f.session(t)
	if s.authPass != "" || containsFold(s.commands, "AUTH") || containsFold(s.commands, "MAIL") {
		t.Fatalf("commands %v: credentials or mail went over a connection without TLS", s.commands)
	}
}

func TestRelayVerifiesCertificate(t *testing.T) {
	f := newFakeSMTP(t)
	rl := f.relay()
	rl.rootCAs = nil // the system roots do not trust the test certificate
	_, serr := rl.submit(context.Background(), testAccount, preparedFixture(t))
	if serr == nil || serr.stage != "starttls" {
		t.Fatalf("err = %+v, want a starttls failure", serr)
	}
	if s := f.session(t); s.authPass != "" {
		t.Fatal("the password was sent to an unverified server")
	}
}

func TestRelayOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opt       func(*fakeSMTP)
		kind      submissionKind
		code      int
		wantData  bool
		wantReset bool
	}{
		{name: "credential refused", opt: func(f *fakeSMTP) { f.authCode = 535 }, kind: submissionCredential, code: 535},
		{name: "auth unavailable", opt: func(f *fakeSMTP) { f.authCode = 454 }, kind: submissionDeferred, code: 454},
		{name: "one recipient rejected", opt: func(f *fakeSMTP) {
			f.rcptReply = func(r string) (int, string) {
				if r == "dave@elsewhere.invalid" {
					return 550, "5.1.1 <dave@elsewhere.invalid>: Recipient address rejected"
				}
				return 250, "2.1.5 Ok"
			}
		}, kind: submissionRejected, code: 550, wantReset: true},
		{name: "recipient deferred", opt: func(f *fakeSMTP) {
			f.rcptReply = func(string) (int, string) { return 451, "4.3.0 try later" }
		}, kind: submissionDeferred, code: 451, wantReset: true},
		{name: "message rejected", opt: func(f *fakeSMTP) { f.dataCode, f.dataMsg = 554, "5.7.1 policy" }, kind: submissionRejected, code: 554, wantData: true},
		{name: "message deferred", opt: func(f *fakeSMTP) { f.dataCode, f.dataMsg = 451, "4.7.1 signer unavailable" }, kind: submissionDeferred, code: 451, wantData: true},
		{name: "no final reply", opt: func(f *fakeSMTP) { f.dropAfterData = true }, kind: submissionUnknown, wantData: true},
		{name: "over the advertised size", opt: func(f *fakeSMTP) { f.size = 64 }, kind: submissionRejected},
		{name: "8-bit without 8BITMIME", opt: func(f *fakeSMTP) { f.no8BitMIME = true }, kind: submissionRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t, tc.opt)
			msg := preparedFixture(t)
			if tc.name == "8-bit without 8BITMIME" {
				msg.eightBit = true
			}
			_, serr := f.relay().submit(context.Background(), testAccount, msg)
			if serr == nil {
				t.Fatal("submit succeeded")
			}
			if serr.kind != tc.kind || serr.code != tc.code {
				t.Fatalf("kind=%s code=%d (%v), want kind=%s code=%d", serr.kind, serr.code, serr, tc.kind, tc.code)
			}
			s := f.session(t)
			if s.gotData != tc.wantData {
				t.Errorf("server received a message = %v, want %v (commands %v)", s.gotData, tc.wantData, s.commands)
			}
			if containsFold(s.commands, "RSET") != tc.wantReset {
				t.Errorf("RSET sent = %v, want %v (commands %v)", !tc.wantReset, tc.wantReset, s.commands)
			}
			if tc.wantReset && containsFold(s.commands, "DATA") {
				t.Errorf("DATA sent after a refused recipient: %v", s.commands)
			}
		})
	}
}

func TestRelayUnreachable(t *testing.T) {
	f := newFakeSMTP(t)
	rl := f.relay()
	rl.address = "127.0.0.1:1" // nothing listens on port 1
	_, serr := rl.submit(context.Background(), testAccount, preparedFixture(t))
	if serr == nil || serr.kind != submissionUnreachable || serr.stage != "connect" {
		t.Fatalf("err = %+v", serr)
	}
	f.noSession(t)
}
