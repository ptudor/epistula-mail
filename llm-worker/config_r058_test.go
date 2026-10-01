package main

import (
	"strings"
	"testing"
)

// TestProductionRejectsOffBoxPlaintextLMStudio is the R-058 regression: in
// production mode a non-loopback LM Studio must be https even when a token is
// set — otherwise the token and full message text cross the LAN in cleartext.
func TestProductionRejectsOffBoxPlaintextLMStudio(t *testing.T) {
	cfg := validatableDefault()
	cfg.Production = true
	cfg.MailAPI.BaseURL = "https://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = "http://192.168.1.10:1234/v1"
	cfg.LMStudio.APIToken = "lm_test"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a plaintext off-box LM Studio in production")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want it to name the https requirement", err)
	}
}

// TestProductionAllowsOffBoxHTTPSLMStudio: an https off-box LM Studio with a
// token is accepted (the tunnel/proxy deployment).
func TestProductionAllowsOffBoxHTTPSLMStudio(t *testing.T) {
	cfg := validatableDefault()
	cfg.Production = true
	cfg.MailAPI.BaseURL = "https://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = "https://lm.example.test/v1"
	cfg.LMStudio.APIToken = "lm_test"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for https off-box LM Studio", err)
	}
}

// TestProductionOffBoxHTTPSStillNeedsToken: https alone off-box is not enough —
// the token requirement is preserved.
func TestProductionOffBoxHTTPSStillNeedsToken(t *testing.T) {
	cfg := validatableDefault()
	cfg.Production = true
	cfg.MailAPI.BaseURL = "https://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = "https://lm.example.test/v1"
	cfg.LMStudio.APIToken = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an off-box https LM Studio with no api_token")
	}
}
