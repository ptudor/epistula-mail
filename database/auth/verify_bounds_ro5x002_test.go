package auth

import (
	"errors"
	"strings"
	"testing"
)

// TestVerifyPasswordMalformedNeverPanics is the RO5X-002 regression.
//
// argon2.IDKey panics rather than erroring on t<1 / p<1 and nil-derefs on a
// zero-length key. password_hash / token_hash are plain TEXT columns with no
// format CHECK, so a bad row must fail auth, not crash the caller.
func TestVerifyPasswordMalformedNeverPanics(t *testing.T) {
	// A known-good hash, used to source a real salt/key pair so each
	// malformed case differs from it in exactly one way.
	good, err := HashPassword("correct horse", Params{
		Memory: 8 * 1024, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32,
	})
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(good, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected PHC shape: %q", good)
	}
	salt, key := parts[4], parts[5]

	phc := func(params, s, k string) string {
		return "$argon2id$v=19$" + params + "$" + s + "$" + k
	}

	cases := []struct {
		name    string
		encoded string
	}{
		{"t=0", phc("m=65536,t=0,p=4", salt, key)},
		{"p=0", phc("m=65536,t=3,p=0", salt, key)},
		{"m=0", phc("m=0,t=3,p=4", salt, key)},
		{"m below argon2 floor", phc("m=4,t=3,p=4", salt, key)},
		{"empty salt", phc("m=65536,t=3,p=4", "", key)},
		{"empty key", phc("m=65536,t=3,p=4", salt, "")},
		{"m=99999999 (≈95 GiB)", phc("m=99999999,t=3,p=4", salt, key)},
		{"t=0 and p=0", phc("m=65536,t=0,p=0", salt, key)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A panic here fails the test rather than killing the process.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("VerifyPassword panicked on %s: %v", tc.name, r)
				}
			}()
			ok, err := VerifyPassword("correct horse", tc.encoded)
			if ok {
				t.Errorf("VerifyPassword returned true for malformed hash %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("err = %v, want ErrInvalidHash", err)
			}
		})
	}
}

// TestVerifyPasswordStillWorksAtConfiguredCosts guards the bounds check
// against over-reach: every cost the deployment can legitimately configure
// must still verify.
func TestVerifyPasswordStillWorksAtConfiguredCosts(t *testing.T) {
	for _, p := range []Params{
		{Memory: 8 * 1024, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 16 * 1024, Iterations: 2, Parallel: 2, SaltLen: 16, KeyLen: 32},
		{Memory: 64 * 1024, Iterations: 3, Parallel: 4, SaltLen: 16, KeyLen: 32},
	} {
		encoded, err := HashPassword("s3kr1t", p)
		if err != nil {
			t.Fatalf("HashPassword(%+v): %v", p, err)
		}
		ok, err := VerifyPassword("s3kr1t", encoded)
		if err != nil || !ok {
			t.Errorf("VerifyPassword at %+v: ok=%v err=%v, want true/nil", p, ok, err)
		}
		ok, err = VerifyPassword("wrong", encoded)
		if err != nil || ok {
			t.Errorf("VerifyPassword(wrong) at %+v: ok=%v err=%v, want false/nil", p, ok, err)
		}
	}
}
