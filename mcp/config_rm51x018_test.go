package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestStrictConfigUnknownKey(t *testing.T) {
	_, err := loadConfig(writeTempConfig(t, "producton = true\n"))
	var strict *toml.StrictMissingError
	if !errors.As(err, &strict) || !strings.Contains(err.Error(), "producton (line 1)") {
		t.Fatalf("error=%v", err)
	}
}

func TestStrictConfigShippedFiles(t *testing.T) {
	t.Setenv("MAIL_MCP_TOKEN", "mapi_test")
	for _, p := range []string{"epistula-mcp.toml.example", "../packaging/epistula-mcp.toml"} {
		if _, err := loadConfig(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestConfigIgnoresWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("epistula-mcp.toml", []byte("producton = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range configSearchPath {
		if !filepath.IsAbs(p) {
			t.Fatalf("relative discovery path %q", p)
		}
	}
	t.Setenv("MAIL_MCP_TOKEN", "mapi_test")
	t.Setenv("MAIL_MCP_BASE_URL", "http://127.0.0.1:8784")
	if _, err := loadConfig(""); err != nil {
		t.Fatal(err)
	}
}
