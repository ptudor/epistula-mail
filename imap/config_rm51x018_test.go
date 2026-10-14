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
	p := filepath.Join(t.TempDir(), "config.toml")
	for _, input := range []string{"producton = true\n", "[postgres]\nstatment_timeout = '5s'\n"} {
		if err := os.WriteFile(p, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(p)
		var strict *toml.StrictMissingError
		if !errors.As(err, &strict) || !strings.Contains(err.Error(), "line") {
			t.Fatalf("error=%v", err)
		}
		for _, e := range strict.Errors {
			if !strings.Contains(err.Error(), strings.Join(e.Key(), ".")) {
				t.Fatal(err)
			}
		}
	}
}

func TestStrictConfigShippedFiles(t *testing.T) {
	for _, p := range []string{"epistula-imap.toml.example", "../packaging/epistula-imap.toml"} {
		if err := loadTOML(p, DefaultConfig()); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if code := runCheckConfig([]string{"-config", "epistula-imap.toml.example"}); code != EX_OK {
		t.Fatalf("example check-config=%d", code)
	}
	// The package deliberately has no credentials until the operator configures it.
	if code := runCheckConfig([]string{"-config", "../packaging/epistula-imap.toml"}); code != EX_CONFIG {
		t.Fatalf("unconfigured package check-config=%d", code)
	}
}

func TestConfigIgnoresWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("epistula-imap.toml", []byte("producton = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range defaultConfigPaths {
		if !filepath.IsAbs(p) {
			t.Fatalf("relative discovery path %q", p)
		}
	}
	t.Setenv("IMAP_DATABASE_DSN", "postgres://localhost/mail?sslmode=disable")
	if code := runCheckConfig(nil); code != EX_OK {
		t.Fatalf("check-config=%d", code)
	}
}
