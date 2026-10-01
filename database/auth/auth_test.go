package auth

import (
	"errors"
	"strings"
	"testing"
)

// fastParams keeps tests under a second by using cheap argon2 parameters.
// Production paths use DefaultParams (~64 MiB / 3 iter).
var fastParams = Params{Memory: 8 * 1024, Iterations: 1, Parallel: 1, KeyLen: 16, SaltLen: 8}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("correcthorsebatterystaple", fastParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("encoded hash missing prefix: %q", hash)
	}
	ok, err := VerifyPassword("correcthorsebatterystaple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword returned false for correct password")
	}
}

func TestVerifyPasswordRejectsWrong(t *testing.T) {
	hash, _ := HashPassword("the right one", fastParams)
	ok, err := VerifyPassword("a different password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("VerifyPassword returned true for wrong password")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"$bcrypt$something",
		"$argon2id$",
		"$argon2id$v=19$",
		"$argon2id$v=19$m=8192,t=1,p=1$notbase64$notbase64",
		"$argon2id$v=99$m=8192,t=1,p=1$xxxx$xxxx",
	}
	for _, c := range cases {
		ok, err := VerifyPassword("any", c)
		if err == nil {
			t.Errorf("VerifyPassword(%q): expected error", c)
		}
		if ok {
			t.Errorf("VerifyPassword(%q): expected false", c)
		}
		if err != nil && !errors.Is(err, ErrInvalidHash) {
			t.Errorf("VerifyPassword(%q): err = %v, want ErrInvalidHash", c, err)
		}
	}
}

func TestHashEmptyPasswordErrors(t *testing.T) {
	_, err := HashPassword("", fastParams)
	if !errors.Is(err, ErrEmptyPassword) {
		t.Errorf("HashPassword(\"\"): err = %v, want ErrEmptyPassword", err)
	}
}

func TestHashesAreSaltedUnique(t *testing.T) {
	h1, _ := HashPassword("same", fastParams)
	h2, _ := HashPassword("same", fastParams)
	if h1 == h2 {
		t.Errorf("two hashes of same password are identical (no salt?): %s", h1)
	}
}
