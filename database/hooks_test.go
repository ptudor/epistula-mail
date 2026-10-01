package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPostDeliveryHookNoopWhenUnset(t *testing.T) {
	cfg := DefaultConfig()
	// Default config leaves PostHookCommand nil; calling the runner
	// should be a fast no-op (no panic, no file writes).
	runPostDeliveryHook(cfg, PostDeliveryHookContext{})
}

// TestPostDeliveryHookExecutesAndPassesEnv writes a tiny shell hook that
// records its env into a tmpfile, runs runPostDeliveryHook, then verifies
// the expected MAIL_DB_* variables landed.
func TestPostDeliveryHookExecutesAndPassesEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook test requires a POSIX shell")
	}
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\n" +
		"{\n" +
		"  echo from=$MAIL_DB_ENVELOPE_FROM\n" +
		"  echo to=$MAIL_DB_ENVELOPE_TO\n" +
		"  echo mailbox=$MAIL_DB_MAILBOX_ID\n" +
		"  echo uid=$MAIL_DB_UID\n" +
		"  echo sha=$MAIL_DB_RAW_SHA256\n" +
		"  echo bytes=$MAIL_DB_RAW_BYTES\n" +
		"  echo att=$MAIL_DB_ATTACHMENTS\n" +
		"  echo catchall=$MAIL_DB_IS_CATCHALL\n" +
		"} > " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Delivery.PostHookCommand = []string{script}
	cfg.Delivery.PostHookTimeout = "5s"

	runPostDeliveryHook(cfg, PostDeliveryHookContext{
		EnvelopeFrom: "alice@example.invalid",
		EnvelopeTo:   "bob@ptudor.invalid",
		MailboxID:    42,
		FolderID:     7,
		UID:          1001,
		MessageID:    9999,
		ModSeq:       12,
		RawSHA256Hex: "deadbeefcafebabe1122334455667788aabbccddeeff00112233445566778899",
		RawSize:      1234,
		Attachments:  2,
		IsCatchall:   true,
	})

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("hook did not produce output: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"from=alice@example.invalid",
		"to=bob@ptudor.invalid",
		"mailbox=42",
		"uid=1001",
		"bytes=1234",
		"att=2",
		"catchall=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q\n---\n%s", want, got)
		}
	}
}

// TestPostDeliveryHookTimeoutDoesNotBlockDelivery verifies a hook that
// sleeps past its deadline is killed and the runner returns. Without the
// timeout enforcement a wedged hook would block the LDA forever.
func TestPostDeliveryHookTimeoutDoesNotBlockDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook test requires a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write slow hook: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Delivery.PostHookCommand = []string{script}
	cfg.Delivery.PostHookTimeout = "200ms"

	done := make(chan struct{})
	go func() {
		runPostDeliveryHook(cfg, PostDeliveryHookContext{})
		close(done)
	}()
	select {
	case <-done:
		// good: runner returned promptly after killing the sleeping hook.
	case <-time.After(5 * time.Second):
		t.Fatal("hook runner did not return after deadline; would block LDA")
	}
}

func TestPostHookConfigRequiresAbsolutePath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Delivery.PostHookCommand = []string{"relative-path.sh"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for non-absolute hook path")
	}
}

func TestPostHookConfigRejectsBadTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Delivery.PostHookCommand = []string{"/bin/true"}
	cfg.Delivery.PostHookTimeout = "potato"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unparseable timeout")
	}
}
