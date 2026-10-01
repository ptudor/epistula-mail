// Package auth implements Argon2id password hashing and verification using
// the PHC-encoded hash format (https://github.com/P-H-C/phc-string-format).
// The encoded form carries its own parameters, so server-side cost tuning is
// a one-line config change rather than a schema migration.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. Memory is in KiB. The defaults
// returned by DefaultParams match OWASP's 2024 recommendation for
// interactive password verification.
type Params struct {
	Memory     uint32
	Iterations uint32
	Parallel   uint8
	KeyLen     uint32
	SaltLen    uint32
}

// DefaultParams returns sensible defaults: 64 MiB / 3 iterations / 4 lanes.
func DefaultParams() Params {
	return Params{
		Memory:     64 * 1024,
		Iterations: 3,
		Parallel:   4,
		KeyLen:     32,
		SaltLen:    16,
	}
}

// ErrInvalidHash is returned when the encoded hash cannot be parsed.
var ErrInvalidHash = errors.New("auth: invalid encoded hash")

// ErrEmptyPassword is returned when the password input is empty.
var ErrEmptyPassword = errors.New("auth: empty password")

// HashPassword returns a PHC-encoded Argon2id hash of password using p.
// The salt is freshly generated from crypto/rand.
func HashPassword(password string, p Params) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}
	if p.SaltLen == 0 {
		p.SaltLen = 16
	}
	if p.KeyLen == 0 {
		p.KeyLen = 32
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallel, p.KeyLen)
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.Memory, p.Iterations, p.Parallel,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
	return encoded, nil
}

// Cost bounds for a hash this package will verify.
//
// The lower bounds keep argon2.IDKey from panicking: it PANICS (it does not
// return an error) on out-of-range parameters — t < 1 is "number of rounds too
// small", p < 1 is "parallelism degree too low", and a zero-length key segment
// nil-derefs. These values come straight out of a TEXT column with no format
// CHECK, so a hand-run psql fix-up, a partial restore, or a different Argon2
// binding writing the row is enough to reach them (RO5X-002).
//
// The upper bounds cap the work one verification can demand BEFORE any memory
// is allocated or any hashing starts. A caller that rations concurrent
// verification (epistula-imap's global authentication budget, epistula-api's
// argonSem) can only ration what it can predict, and a single malformed or
// imported PHC row asking for gigabytes of scratch memory or thousands of
// iterations would monopolize a rationed slot no matter how the slot was
// sized (RA6X-023).
//
// The limits are far above anything a deployment produces. The default
// is 64 MiB / t=3 / p=4 with a 16-byte salt and 32-byte key, and admin
// configuration is floored at 16 MiB in production; MaxMemoryKiB is 16x the
// default and MaxIterations 30x it. A hash outside these bounds is rejected as
// malformed, which for an account means its owner must have the password reset
// with the current parameters — the same migration path as any other
// unparseable hash.
const (
	MinMemoryKiB  = 8
	MaxMemoryKiB  = 1024 * 1024 // 1 GiB of Argon2 scratch memory
	MaxIterations = 100
	MaxParallel   = 64
	MaxSaltLen    = 1024
	MaxKeyLen     = 1024
)

// EncodedParams is the cost a PHC-encoded hash will demand when verified,
// recovered without doing the work. Callers that ration concurrent
// verification use Memory to size their reservation.
type EncodedParams struct {
	Memory     uint32 // KiB of scratch memory argon2.IDKey will allocate
	Iterations uint32
	Parallel   uint8
	SaltLen    int
	KeyLen     int
}

// ParseEncodedParams validates a PHC-encoded Argon2id hash and returns the
// cost it will demand, WITHOUT hashing anything. It performs exactly the
// checks VerifyPassword performs before it starts work, so
// "ParseEncodedParams succeeded" means "VerifyPassword will not reject this
// hash as malformed".
//
// This exists so a rate-limited caller can decide whether it can afford a
// verification, and how much budget to reserve, before committing to it. A
// hostile or corrupted row is rejected here at parse cost.
func ParseEncodedParams(encoded string) (EncodedParams, error) {
	_, p, err := parseEncoded(encoded)
	return p, err
}

func parseEncoded(encoded string) (decoded struct{ Salt, Key []byte }, p EncodedParams, err error) {
	// Refuse excessive salt/key encodings before Split/Decode allocate them.
	if len(encoded) > 4096 {
		return decoded, p, ErrInvalidHash
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return decoded, p, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return decoded, p, fmt.Errorf("%w: version: %v", ErrInvalidHash, err)
	}
	if version != argon2.Version {
		return decoded, p, fmt.Errorf("%w: unsupported argon2 version %d", ErrInvalidHash, version)
	}

	var memory, iterations uint32
	var parallel uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallel); err != nil {
		return decoded, p, fmt.Errorf("%w: params: %v", ErrInvalidHash, err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return decoded, p, fmt.Errorf("%w: salt: %v", ErrInvalidHash, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return decoded, p, fmt.Errorf("%w: hash: %v", ErrInvalidHash, err)
	}

	if memory < MinMemoryKiB || memory > MaxMemoryKiB ||
		iterations < 1 || iterations > MaxIterations ||
		parallel < 1 || parallel > MaxParallel ||
		len(salt) == 0 || len(salt) > MaxSaltLen ||
		len(key) == 0 || len(key) > MaxKeyLen {
		return decoded, p, fmt.Errorf("%w: out-of-range parameters", ErrInvalidHash)
	}

	decoded.Salt = salt
	decoded.Key = key
	return decoded, EncodedParams{
		Memory:     memory,
		Iterations: iterations,
		Parallel:   parallel,
		SaltLen:    len(salt),
		KeyLen:     len(key),
	}, nil
}

// VerifyPassword compares password against a PHC-encoded Argon2id hash in
// constant time. Returns (true, nil) on match, (false, nil) on mismatch, and
// a non-nil error for malformed encoded input. The cost bounds documented on
// MaxMemoryKiB are enforced before any hashing begins, so VerifyPassword is
// total and every caller's "non-nil error == auth failure" contract holds.
func VerifyPassword(password, encoded string) (bool, error) {
	decoded, p, err := parseEncoded(encoded)
	if err != nil {
		return false, err
	}
	salt, key := decoded.Salt, decoded.Key
	iterations, memory, parallel := p.Iterations, p.Memory, p.Parallel

	candidate := argon2.IDKey([]byte(password), salt, iterations, memory, parallel, uint32(len(key)))
	if subtle.ConstantTimeCompare(candidate, key) == 1 {
		return true, nil
	}
	return false, nil
}
