package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSigned generates a throwaway self-signed cert for cn and writes
// PEM cert+key to the given paths.
func writeSelfSigned(t *testing.T, cn, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func leafCN(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf.Subject.CommonName
}

func TestCertReloaderSwapsOnReload(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	writeSelfSigned(t, "first.invalid", certPath, keyPath)
	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	got, err := r.GetCertificate(nil)
	if err != nil || got == nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cn := leafCN(t, got); cn != "first.invalid" {
		t.Fatalf("initial cert CN = %q, want first.invalid", cn)
	}

	// Renewal: overwrite the files and SIGHUP-equivalent Reload.
	writeSelfSigned(t, "renewed.invalid", certPath, keyPath)
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got, _ = r.GetCertificate(nil)
	if cn := leafCN(t, got); cn != "renewed.invalid" {
		t.Fatalf("post-reload cert CN = %q, want renewed.invalid", cn)
	}
}

func TestCertReloaderKeepsCurrentOnFailedReload(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	writeSelfSigned(t, "stable.invalid", certPath, keyPath)
	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}

	// Botched renewal: corrupt the on-disk cert, then Reload.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil {
		t.Fatal("Reload of corrupt keypair succeeded, want error")
	}
	got, _ := r.GetCertificate(nil)
	if got == nil {
		t.Fatal("GetCertificate returned nil after failed reload")
	}
	if cn := leafCN(t, got); cn != "stable.invalid" {
		t.Fatalf("cert after failed reload CN = %q, want stable.invalid (keep serving the old cert)", cn)
	}
}

func TestCertReloaderFailsClosedAtStartup(t *testing.T) {
	dir := t.TempDir()
	if _, err := newCertReloader(filepath.Join(dir, "missing.pem"), filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("newCertReloader with missing files succeeded, want startup failure")
	}
}
