package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPostDeliveryHookKillsGroupAndFiltersEnv(t *testing.T) {
	t.Setenv("MAIL_DATABASE_DSN", "test-secret-not-for-hook")
	t.Setenv("MAIL_DATABASE_STORAGE_ROOT", "/not-for-hook")
	dir := t.TempDir()
	envFile, pidFile := filepath.Join(dir, "env"), filepath.Join(dir, "pid")
	c := DefaultConfig()
	c.Delivery.PostHookTimeout = "200ms"
	c.Delivery.PostHookCommand = []string{"/bin/sh", "-c", `env > "$1"; sleep 30 & child=$!; echo "$child" > "$2"; wait`, "hook", envFile, pidFile}
	start := time.Now()
	runPostDeliveryHook(c, PostDeliveryHookContext{MessageID: 123})
	if time.Since(start) > 2*time.Second {
		t.Fatal("hook timeout was not prompt")
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "MAIL_DATABASE_") || !strings.Contains(string(env), "MAIL_DB_MESSAGE_ID=123") {
		t.Fatalf("unexpected hook environment: %s", env)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("hook grandchild survived process-group cancellation")
}
