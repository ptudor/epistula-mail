package main

import (
	"strings"
	"testing"
)

func TestValidatePasswordEnforcesMinimum(t *testing.T) {
	if _, err := validatePassword("short", 12, 1024); err == nil {
		t.Fatal("expected min-length rejection, got nil")
	} else if !strings.Contains(err.Error(), "at least 12") {
		t.Fatalf("want min-length error mentioning the floor, got %v", err)
	}
}

func TestValidatePasswordAcceptsExactlyMin(t *testing.T) {
	pw := strings.Repeat("a", 12)
	got, err := validatePassword(pw, 12, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != pw {
		t.Fatalf("validatePassword returned %q; want %q", got, pw)
	}
}

func TestValidatePasswordEnforcesMaximum(t *testing.T) {
	pw := strings.Repeat("a", 2048)
	if _, err := validatePassword(pw, 12, 1024); err == nil {
		t.Fatal("expected max-length rejection, got nil")
	}
}

func TestValidatePasswordRejectsNUL(t *testing.T) {
	pw := "longenoughpasswordwithNUL\x00inside"
	if _, err := validatePassword(pw, 12, 1024); err == nil {
		t.Fatal("expected NUL-byte rejection, got nil")
	}
}

func TestAdminConfigValidatesPasswordPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Admin.MinPasswordLength = 4
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for min_password_length < 8")
	}

	cfg = DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/mail?sslmode=disable"
	cfg.Admin.MaxPasswordLength = cfg.Admin.MinPasswordLength
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when max <= min")
	}
}
