package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// submissionFixture is the API fixture with alice able to send through a fake
// submission server. alice owns alice@mail.test.invalid and the domain's
// catchall; bob owns bob@mail.test.invalid but has no submission account.
type submissionFixture struct {
	*apiFixture
	smtp       *fakeSMTP
	aliceSend  string // scope alice, submit_message
	bobSend    string // scope bob, submit_message
	allSend    string // scope *, submit_message
	aliceWrite string // scope alice, read_content + write_annotation, no submit_message
}

func newSubmissionFixture(t *testing.T, opts ...func(*fakeSMTP)) *submissionFixture {
	t.Helper()
	f := &submissionFixture{apiFixture: newAPIFixture(t), smtp: newFakeSMTP(t, opts...)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `
		WITH d AS (INSERT INTO domains (name) VALUES ('mail.test.invalid') RETURNING id)
		INSERT INTO aliases (domain_id, localpart, mailbox_id)
		SELECT d.id, v.localpart, mb.id
		  FROM d, (VALUES ('alice', 'alice'), ('', 'alice'), ('bob', 'bob')) AS v(localpart, mailbox)
		  JOIN mailboxes mb ON mb.name = v.mailbox`); err != nil {
		t.Fatalf("aliases: %v", err)
	}
	cfg := testSubmissionConfig()
	cfg.Accounts = []SubmissionAccount{{Mailbox: "alice", Username: "alice", Password: "test-only-password"}}
	cfg.MessagesPerHour = 2
	f.srv.cfg.Submission = cfg
	f.srv.relay = f.smtp.relay()
	f.srv.submitLimiter = newRateLimiter(float64(cfg.MessagesPerHour)/3600, cfg.MessagesPerHour)

	f.aliceSend = f.mintToken(ctx, "alice-send", []string{"alice"}, []string{auth.PermissionSubmitMessage})
	f.bobSend = f.mintToken(ctx, "bob-send", []string{"bob"}, []string{auth.PermissionSubmitMessage})
	f.allSend = f.mintToken(ctx, "all-send", []string{"*"}, []string{auth.PermissionSubmitMessage})
	f.aliceWrite = f.mintToken(ctx, "alice-write", []string{"alice"},
		[]string{auth.PermissionReadContent, auth.PermissionWriteAnnotation, auth.PermissionWriteClassification})
	return f
}

func (f *submissionFixture) submit(mailbox, token string, body any) *http.Response {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(http.MethodPost, "/v1/mailboxes/"+mailbox+"/submissions", token, bytes.NewReader(raw))
}

func messageBody(msg string, extra map[string]any) map[string]any {
	b := map[string]any{"message_base64": []byte(msg)}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func TestSubmissionEndToEnd(t *testing.T) {
	f := newSubmissionFixture(t)
	resp := f.submit("alice", f.aliceSend, messageBody(signedFixture, nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var got submissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := crlf(signedFixture)
	sum := sha256.Sum256(want)
	if got.QueueID != "4F2D61A3B7" || got.Mailbox != "alice" || got.EnvelopeFrom != "alice@mail.test.invalid" ||
		got.MessageID != "<m1@mail.test.invalid>" || got.SizeBytes != len(want) ||
		got.SHA256 != hex.EncodeToString(sum[:]) || !got.LineEndingsNormalized {
		t.Errorf("response = %+v", got)
	}
	if len(got.Recipients) != 3 {
		t.Errorf("recipients = %v", got.Recipients)
	}
	s := f.smtp.session(t)
	if !bytes.Equal(s.data, want) {
		t.Errorf("the server received %q", s.data)
	}
	// Submitting writes nothing to the store.
	var annotations, classifications int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM message_annotations), (SELECT count(*) FROM message_classifications)`,
	).Scan(&annotations, &classifications); err != nil {
		t.Fatal(err)
	}
	if annotations+classifications != 0 {
		t.Errorf("a submission wrote %d sidecar rows", annotations+classifications)
	}
}

func TestSubmissionAuthorization(t *testing.T) {
	f := newSubmissionFixture(t)
	other := "From: bob@mail.test.invalid\r\nTo: x@elsewhere.invalid\r\nDate: Sat, 24 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n"
	catchall := strings.Replace(other, "bob@", "anything@", 1)
	spoofed := strings.Replace(other, "bob@mail.test.invalid", "ceo@bank.invalid", 1)
	for _, tc := range []struct {
		name    string
		mailbox string
		token   string
		body    map[string]any
		status  int
		detail  string
	}{
		{"no submit_message", "alice", f.aliceWrite, messageBody(signedFixture, nil), http.StatusForbidden, "submit_message"},
		{"read token", "alice", f.aliceContentToken, messageBody(signedFixture, nil), http.StatusForbidden, "submit_message"},
		{"out of scope", "alice", f.bobSend, messageBody(signedFixture, nil), http.StatusForbidden, "not authorized for this mailbox"},
		{"absent mailbox, scoped token", "nobody", f.aliceSend, messageBody(signedFixture, nil), http.StatusForbidden, "not authorized"},
		{"absent mailbox, all-mailbox token", "nobody", f.allSend, messageBody(signedFixture, nil), http.StatusNotFound, "does not exist"},
		{"no submission account", "bob", f.bobSend, messageBody(other, nil), http.StatusServiceUnavailable, "not configured for mailbox 'bob'"},
		{"From of another mailbox", "alice", f.aliceSend, messageBody(other, nil), http.StatusForbidden, "bob@mail.test.invalid is not an address of mailbox 'alice'"},
		{"catchall From", "alice", f.aliceSend, messageBody(catchall, nil), http.StatusForbidden, "a catchall does not count"},
		{"foreign From", "alice", f.aliceSend, messageBody(spoofed, nil), http.StatusForbidden, "ceo@bank.invalid"},
		{"foreign envelope sender", "alice", f.aliceSend,
			messageBody(signedFixture, map[string]any{"envelope_from": "bob@mail.test.invalid"}), http.StatusForbidden, "bob@mail.test.invalid"},
		{"invalid message", "alice", f.aliceSend, messageBody("not a message", nil), http.StatusUnprocessableEntity, ""},
		{"unknown field", "alice", f.aliceSend, messageBody(signedFixture, map[string]any{"bcc": []string{"x@e.invalid"}}), http.StatusBadRequest, "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := wantProblem(t, f.submit(tc.mailbox, tc.token, tc.body), tc.status)
			if !strings.Contains(p.Detail, tc.detail) {
				t.Errorf("detail %q, want it to mention %q", p.Detail, tc.detail)
			}
		})
	}
	f.smtp.noSession(t)

	// A disabled mailbox cannot send, even with a valid token and account.
	if _, err := f.pool.Exec(context.Background(), `UPDATE mailboxes SET disabled_at = now() WHERE name = 'alice'`); err != nil {
		t.Fatal(err)
	}
	p := wantProblem(t, f.submit("alice", f.aliceSend, messageBody(signedFixture, nil)), http.StatusForbidden)
	if !strings.Contains(p.Detail, "disabled") {
		t.Errorf("disabled mailbox detail %q", p.Detail)
	}
	f.smtp.noSession(t)
}

func TestSubmissionMessageBudget(t *testing.T) {
	f := newSubmissionFixture(t)
	for i := 0; i < 2; i++ {
		resp := f.submit("alice", f.aliceSend, messageBody(signedFixture, nil))
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("message %d: status %d", i+1, resp.StatusCode)
		}
		f.smtp.session(t)
	}
	p := wantProblem(t, f.submit("alice", f.aliceSend, messageBody(signedFixture, nil)), http.StatusTooManyRequests)
	if !strings.Contains(p.Detail, "nothing was sent") {
		t.Errorf("detail %q", p.Detail)
	}
	f.smtp.noSession(t)
}

func TestSubmissionFailureStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opt    func(*fakeSMTP)
		status int
		slug   string
		detail string
	}{
		{"recipient rejected", func(s *fakeSMTP) {
			s.rcptReply = func(r string) (int, string) {
				if r == "dave@elsewhere.invalid" {
					return 550, "5.1.1 <dave@elsewhere.invalid>: Recipient address rejected"
				}
				return 250, "2.1.5 Ok"
			}
		}, http.StatusUnprocessableEntity, "submission-rejected", "Nothing was sent: the submission service refused recipient dave@elsewhere.invalid (SMTP 550"},
		{"deferred", func(s *fakeSMTP) { s.dataCode, s.dataMsg = 451, "4.7.1 signer unavailable" },
			http.StatusServiceUnavailable, "submission-deferred", "The refusal is temporary"},
		{"credential", func(s *fakeSMTP) { s.authCode = 535 },
			http.StatusBadGateway, "submission-failed", "refused this server's account for mailbox 'alice' (SMTP 535"},
		{"unknown", func(s *fakeSMTP) { s.dropAfterData = true },
			http.StatusGatewayTimeout, "submission-outcome-unknown", "may have been sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubmissionFixture(t, tc.opt)
			resp := f.submit("alice", f.aliceSend, messageBody(signedFixture, nil))
			p := wantProblem(t, resp, tc.status)
			if !strings.HasSuffix(p.Type, "/"+tc.slug) || !strings.Contains(p.Detail, tc.detail) {
				t.Errorf("problem %+v, want slug %s and detail containing %q", p, tc.slug, tc.detail)
			}
			if strings.Contains(p.Detail, "test-only-password") {
				t.Fatal("a problem detail echoed the account password")
			}
			f.smtp.session(t)
		})
	}
}
