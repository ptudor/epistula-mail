package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestAPITokenRoundTrip(t *testing.T) {
	secret, err := GenerateAPITokenSecret()
	if err != nil {
		t.Fatalf("GenerateAPITokenSecret: %v", err)
	}
	if len(secret) != 43 {
		t.Errorf("secret length = %d, want 43 (256 bits base64url unpadded)", len(secret))
	}
	tok := FormatAPIToken(42, secret)
	if !strings.HasPrefix(tok, "mapi_42_") {
		t.Errorf("token = %q, want mapi_42_ prefix", tok)
	}
	id, parsedSecret, err := ParseAPIToken(tok)
	if err != nil {
		t.Fatalf("ParseAPIToken: %v", err)
	}
	if id != 42 || parsedSecret != secret {
		t.Errorf("parsed (%d, %q), want (42, %q)", id, parsedSecret, secret)
	}
}

func TestAPITokenSecretsAreUnique(t *testing.T) {
	a, _ := GenerateAPITokenSecret()
	b, _ := GenerateAPITokenSecret()
	if a == b {
		t.Fatal("two generated secrets are identical")
	}
}

func TestAPITokenHashVerifies(t *testing.T) {
	secret, _ := GenerateAPITokenSecret()
	hash, err := HashPassword(secret, DefaultParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(secret, hash)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword = (%v, %v), want (true, nil)", ok, err)
	}
	other, _ := GenerateAPITokenSecret()
	ok, err = VerifyPassword(other, hash)
	if err != nil || ok {
		t.Fatalf("VerifyPassword with wrong secret = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestParseAPITokenRejectsMalformed(t *testing.T) {
	for _, tok := range []string{
		"",
		"mapi_",
		"mapi_1",
		"mapi_1_",
		"mapi__secret",
		"mapi_0_secret",
		"mapi_-3_secret",
		"mapi_abc_secret",
		"other_1_secret",
		"1_secret",
	} {
		if _, _, err := ParseAPIToken(tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("ParseAPIToken(%q) err = %v, want ErrInvalidToken", tok, err)
		}
	}
}

func TestValidPermission(t *testing.T) {
	for _, p := range []string{PermissionReadMetadata, PermissionReadContent, PermissionWriteAnnotation} {
		if !ValidPermission(p) {
			t.Errorf("ValidPermission(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "admin", "READ_METADATA", "read"} {
		if ValidPermission(p) {
			t.Errorf("ValidPermission(%q) = true, want false", p)
		}
	}
}
