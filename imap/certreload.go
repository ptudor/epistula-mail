package main

import (
	"crypto/tls"
	"fmt"
	"sync/atomic"
)

// certReloader holds the server TLS certificate behind an atomic pointer so
// a SIGHUP can swap in a renewed certificate (Let's Encrypt rotation)
// without dropping live connections: new handshakes pick up the new cert
// via GetCertificate; established sessions finish on the old one.
type certReloader struct {
	certPath string
	keyPath  string
	cert     atomic.Pointer[tls.Certificate]
}

// newCertReloader loads the initial keypair; a failure here is fatal at
// startup (fail closed — never start without a serving certificate).
func newCertReloader(certPath, keyPath string) (*certReloader, error) {
	r := &certReloader{certPath: certPath, keyPath: keyPath}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the keypair from disk and atomically swaps it in. On
// failure the previous certificate stays active — a botched renewal
// degrades to "old cert until fixed", never to an outage.
func (r *certReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load TLS keypair: %w", err)
	}
	r.cert.Store(&cert)
	return nil
}

// GetCertificate is the tls.Config callback; every new handshake reads the
// current pointer.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cert.Load(), nil
}

// aeadCipherSuites is the TLS 1.2 cipher allowlist from CLAUDE.md: ECDHE +
// AEAD only, no CBC, no RSA key exchange. Go ignores this list for TLS 1.3
// (1.3 suites are all AEAD and not configurable).
var aeadCipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}
