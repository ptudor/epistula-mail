// API bearer tokens for api.
//
// A presented token is "mapi_<id>_<secret>": the api_tokens row id plus a
// 256-bit random secret. Embedding the id makes verification a single-row
// lookup followed by one Argon2id computation — never a scan over every
// stored hash. Only the Argon2id PHC hash of the secret is stored; the
// presented form exists exactly once, on the terminal of the operator who
// minted it.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TokenPrefix namespaces presented tokens so a leaked value is recognizable
// in scanning tools and secret-detection hooks.
const TokenPrefix = "mapi_"

// ErrInvalidToken is returned for any presented token that does not parse
// as "mapi_<id>_<secret>". Callers should treat it exactly like a failed
// hash verification (HTTP 401), never as a distinct error class visible to
// the client.
var ErrInvalidToken = errors.New("auth: invalid api token")

// API token permission levels. The api_tokens.permissions CHECK constraint
// mirrors this set; keep both in sync.
//
// write_classification is separate from write_annotation because a
// classification files mail: the archive sorter moves a message into the
// category it names (migration 020). An annotation is advisory; the
// interactive epistula-mcp connector may hold write_annotation, and must not
// thereby be able to decide where mail goes.
const (
	PermissionReadMetadata        = "read_metadata"
	PermissionReadContent         = "read_content"
	PermissionWriteAnnotation     = "write_annotation"
	PermissionWriteClassification = "write_classification"
)

// ValidPermission reports whether p is one of the defined permission levels.
func ValidPermission(p string) bool {
	switch p {
	case PermissionReadMetadata, PermissionReadContent, PermissionWriteAnnotation, PermissionWriteClassification:
		return true
	}
	return false
}

// GenerateAPITokenSecret returns a fresh 256-bit random secret encoded as
// unpadded base64url (43 characters).
func GenerateAPITokenSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// FormatAPIToken renders the presented form handed to the operator once at
// mint time.
func FormatAPIToken(id int64, secret string) string {
	return TokenPrefix + strconv.FormatInt(id, 10) + "_" + secret
}

// ParseAPIToken splits a presented token into its row id and secret.
// The secret's hash comparison is the caller's job (VerifyPassword).
func ParseAPIToken(token string) (id int64, secret string, err error) {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return 0, "", ErrInvalidToken
	}
	idStr, secret, ok := strings.Cut(rest, "_")
	if !ok || secret == "" {
		return 0, "", ErrInvalidToken
	}
	id, convErr := strconv.ParseInt(idStr, 10, 64)
	if convErr != nil || id <= 0 {
		return 0, "", ErrInvalidToken
	}
	return id, secret, nil
}
