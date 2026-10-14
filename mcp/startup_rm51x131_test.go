package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryConfigExitCodes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "epistula-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cfg := writeTempConfig(t, "[mailapi]\nbase_url=\"http://127.0.0.1:8784\"\ntoken=\"test-read\"\n")
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"missing config", []string{"-config", filepath.Join(t.TempDir(), "absent")}, EX_CONFIG},
		{"loopback without token", []string{"-config", cfg, "-http", "127.0.0.1:0"}, EX_CONFIG},
		{"nonloopback without token", []string{"-config", cfg, "-http", "0.0.0.0:0"}, EX_CONFIG},
		{"listener failure", []string{"-config", cfg, "-http", "127.0.0.1:invalid"}, EX_OSERR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, "MAIL_MCP_") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			if tc.code == EX_OSERR {
				cmd.Env = append(cmd.Env, "MAIL_MCP_HTTP_TOKEN=test-http")
			}
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
				t.Fatalf("exit=%v, want %d: %s", err, tc.code, out)
			}
		})
	}
}
