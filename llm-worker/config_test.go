package main

import "testing"

// validatableDefault returns DefaultConfig with the annotation-model
// derivation LoadConfig applies before Validate — so tests that call Validate
// directly (bypassing LoadConfig) still have the derived annotation key that
// Validate requires. See R-011: the raw default leaves AnnotationModel empty.
func validatableDefault() *Config {
	cfg := DefaultConfig()
	cfg.deriveAnnotationModel()
	return cfg
}

func TestDefaultConfigValidWithToken(t *testing.T) {
	cfg := validatableDefault()
	cfg.MailAPI.BaseURL = "https://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestProductionRequiresSecureMailAPI(t *testing.T) {
	cfg := validatableDefault()
	cfg.Production = true
	cfg.MailAPI.BaseURL = "http://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() succeeded for insecure production epistula-api URL")
	}
}

func TestProductionAllowsLoopbackLMStudioWithoutToken(t *testing.T) {
	cfg := validatableDefault()
	cfg.Production = true
	cfg.MailAPI.BaseURL = "https://mail.example.test"
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = "http://127.0.0.1:1234/v1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}
