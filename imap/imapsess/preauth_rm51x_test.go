package imapsess

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestPreAuthTimeoutLogsAndCountsOnce(t *testing.T) {
	var logs bytes.Buffer
	be := &Backend{Logger: slog.New(slog.NewTextHandler(&logs, nil)), PreAuthTimeout: 20 * time.Millisecond}
	counter := metricLimiterRejections.WithLabelValues("preauth_timeout")
	before := counterValue(t, counter)
	client, server := net.Pipe()
	defer client.Close()
	s := be.NewSessionForNetConn(server)
	client.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := client.Read(b[:]); err == nil {
		t.Fatal("connection remains open")
	}
	s.Close()
	s.Close()
	if !strings.Contains(logs.String(), "pre-authentication timeout") || !strings.Contains(logs.String(), "ip=") {
		t.Fatal(logs.String())
	}
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("timeout count=%f", got)
	}
}

func TestLoginCannotWinAfterPreAuthTimer(t *testing.T) {
	be, _ := authBudgetFixture(t, NewAuthBudget(64*1024))
	var logs bytes.Buffer
	be.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	be.PreAuthTimeout = 25 * time.Millisecond
	be.PerMailbox = NewMailboxSessionLimiter(1)
	if _, err := be.loginProfiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	be.authVerify = func(string, string) (bool, error) { time.Sleep(75 * time.Millisecond); return true, nil }
	success, failure := metricAuthTotal.WithLabelValues("success"), metricAuthTotal.WithLabelValues("failure")
	beforeSuccess, beforeFailure := counterValue(t, success), counterValue(t, failure)
	client, server := net.Pipe()
	defer client.Close()
	s := be.NewSessionForNetConn(server)
	defer s.Close()
	err := s.Login("alice", "rightpw")
	var response *imap.Error
	if !errors.As(err, &response) || response.Code != imap.ResponseCodeUnavailable {
		t.Fatalf("login=%v", err)
	}
	if s.mailboxSlotHeld || s.mailboxID != 0 || strings.Contains(logs.String(), "login ok") {
		t.Fatalf("dead connection authenticated: %s", logs.String())
	}
	if counterValue(t, success) != beforeSuccess || counterValue(t, failure) != beforeFailure {
		t.Fatal("timer loser counted as an authentication outcome")
	}
}
