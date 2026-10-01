package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTOML writes a minimal-but-valid worker config with the given body plus
// the required epistula-api/lmstudio keys, and returns its path.
func writeWorkerTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.toml")
	full := `
[mail_api]
base_url = "https://epistula-api.example.invalid"
token = "mapi_test"

[lmstudio]
` + body
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	return path
}

// TestAnnotationModelDerivedFromLMStudioModel is the R-011 core case: with only
// lmstudio.model set (per QUICKSTART), the annotation key derives to
// "lmstudio:<model>" — not the old hardcoded qwen default.
func TestAnnotationModelDerivedFromLMStudioModel(t *testing.T) {
	t.Setenv("MAIL_LLM_MODEL", "") // ensure no env override
	path := writeWorkerTOML(t, `base_url = "http://127.0.0.1:1234/v1"
model = "openai/gpt-oss-20b"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.Worker.AnnotationModel, "lmstudio:openai/gpt-oss-20b"; got != want {
		t.Fatalf("AnnotationModel = %q, want %q", got, want)
	}
}

// TestAnnotationModelExplicitWins confirms an explicit annotation_model is not
// overridden by the derivation.
func TestAnnotationModelExplicitWins(t *testing.T) {
	t.Setenv("MAIL_LLM_MODEL", "")
	path := writeWorkerTOML(t, `base_url = "http://127.0.0.1:1234/v1"
model = "openai/gpt-oss-20b"

[worker]
annotation_model = "custom:key-x"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.Worker.AnnotationModel, "custom:key-x"; got != want {
		t.Fatalf("AnnotationModel = %q, want %q (explicit TOML must win)", got, want)
	}
}

// TestAnnotationModelExplicitWinsOverEnvModel confirms MAIL_LLM_MODEL changes
// the model used for inference but does NOT clobber an explicit
// annotation_model pinned in TOML.
func TestAnnotationModelExplicitWinsOverEnvModel(t *testing.T) {
	t.Setenv("MAIL_LLM_MODEL", "openai/gpt-oss-20b")
	path := writeWorkerTOML(t, `base_url = "http://127.0.0.1:1234/v1"
model = "qwen/qwen3.6-35b-a3b"

[worker]
annotation_model = "pinned:key"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.Worker.AnnotationModel, "pinned:key"; got != want {
		t.Fatalf("AnnotationModel = %q, want %q (TOML pin must survive MAIL_LLM_MODEL)", got, want)
	}
	// And the env override still took effect for the inference model.
	if got, want := cfg.LMStudio.Model, "openai/gpt-oss-20b"; got != want {
		t.Fatalf("LMStudio.Model = %q, want %q", got, want)
	}
}
