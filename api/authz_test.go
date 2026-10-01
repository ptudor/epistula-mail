package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ptudor/epistula-mail/database/auth"
)

func TestTokenPermissionComposition(t *testing.T) {
	contentOnly := &apiToken{permissions: map[string]bool{auth.PermissionReadContent: true}}
	if !contentOnly.Can(auth.PermissionReadContent) {
		t.Error("read_content token must pass read_content")
	}
	if !contentOnly.Can(auth.PermissionReadMetadata) {
		t.Error("read_content must subsume read_metadata")
	}
	if contentOnly.Can(auth.PermissionWriteAnnotation) {
		t.Error("read_content must not grant write_annotation")
	}

	metaOnly := &apiToken{permissions: map[string]bool{auth.PermissionReadMetadata: true}}
	if metaOnly.Can(auth.PermissionReadContent) {
		t.Error("read_metadata must not grant read_content")
	}
}

func TestTokenScope(t *testing.T) {
	// Scope is durable mailbox IDs, not names (RA6X-012).
	scoped := &apiToken{ScopeMailboxIDs: []int64{11, 13}}
	if !scoped.InScope(11) || scoped.InScope(12) {
		t.Error("scope membership wrong")
	}
	all := &apiToken{AllMailboxes: true}
	if !all.InScope(999) {
		t.Error("* scope must match every mailbox")
	}
	// An ID is never reused, so a scope entry for a deleted mailbox matches
	// nothing — including a mailbox recreated under the deleted one's name,
	// which necessarily has a different ID.
	if scoped.InScope(0) {
		t.Error("a zero/unknown mailbox id must never be in scope")
	}
}

func TestClientIPPrefersApacheXFFOnLoopback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/mailboxes", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.9")
	if ip := clientIP(r); ip != "198.51.100.9" {
		t.Errorf("clientIP = %q, want the last XFF entry (the one Apache appended)", ip)
	}
}

func TestClientIPIgnoresXFFFromNonLoopback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/mailboxes", nil)
	r.RemoteAddr = "192.0.2.10:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if ip := clientIP(r); ip != "192.0.2.10" {
		t.Errorf("clientIP = %q, want the TCP peer when it is not loopback", ip)
	}
}
