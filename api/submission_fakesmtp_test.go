package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal RFC 6409 submission server for relay tests: EHLO,
// STARTTLS, AUTH PLAIN, MAIL, RCPT, RSET, DATA, QUIT. It records each session,
// including the message exactly as it arrived (CRLF kept, dot-stuffing
// undone), so a test can compare it byte for byte with what was submitted.
// Every behaviour knob is set before the accept loop starts.
type fakeSMTP struct {
	ln        net.Listener
	tlsConfig *tls.Config
	rootCAs   *x509.CertPool
	name      string

	noSTARTTLS    bool
	no8BitMIME    bool
	size          int
	authCode      int
	rcptReply     func(rcpt string) (int, string)
	dataCode      int
	dataMsg       string
	dropAfterData bool

	wg       sync.WaitGroup
	sessions chan fakeSession
}

type fakeSession struct {
	helo     string
	tls      bool
	authUser string
	authPass string
	mail     string
	rcpts    []string
	data     []byte
	gotData  bool
	commands []string
}

const fakeSMTPName = "submission.test.invalid"

func newFakeSMTP(t *testing.T, opts ...func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	cert, pool := selfSignedCert(t, fakeSMTPName)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{
		ln:        ln,
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		rootCAs:   pool,
		name:      fakeSMTPName,
		authCode:  235,
		dataCode:  250,
		dataMsg:   "2.0.0 Ok: queued as 4F2D61A3B7",
		sessions:  make(chan fakeSession, 16),
	}
	for _, o := range opts {
		o(f)
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
	})
	return f
}

// relay returns a relay pointed at this server that trusts its certificate.
func (f *fakeSMTP) relay() *submissionRelay {
	return &submissionRelay{
		address:    f.ln.Addr().String(),
		serverName: f.name,
		heloName:   "api.test.invalid",
		timeout:    5 * time.Second,
		rootCAs:    f.rootCAs,
		dial:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
}

// session waits for the next finished session.
func (f *fakeSMTP) session(t *testing.T) fakeSession {
	t.Helper()
	select {
	case s := <-f.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no SMTP session finished")
		return fakeSession{}
	}
}

// noSession asserts that no session reached the server at all.
func (f *fakeSMTP) noSession(t *testing.T) {
	t.Helper()
	select {
	case s := <-f.sessions:
		t.Fatalf("unexpected SMTP session: %+v", s.commands)
	case <-time.After(100 * time.Millisecond):
	}
}

func (f *fakeSMTP) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(conn)
		}()
	}
}

func (f *fakeSMTP) handle(raw net.Conn) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	var conn net.Conn = raw
	r := bufio.NewReader(conn)
	sess := fakeSession{}
	defer func() { f.sessions <- sess }()
	reply := func(lines ...string) {
		for _, l := range lines {
			fmt.Fprintf(conn, "%s\r\n", l)
		}
	}
	reply("220 " + f.name + " ESMTP fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		sess.commands = append(sess.commands, verb)
		switch verb {
		case "EHLO":
			sess.helo = arg
			ext := []string{f.name}
			if f.size > 0 {
				ext = append(ext, fmt.Sprintf("SIZE %d", f.size))
			}
			if !f.no8BitMIME {
				ext = append(ext, "8BITMIME", "SMTPUTF8")
			}
			if !sess.tls && !f.noSTARTTLS {
				ext = append(ext, "STARTTLS")
			}
			if sess.tls {
				ext = append(ext, "AUTH PLAIN LOGIN")
			}
			ext = append(ext, "ENHANCEDSTATUSCODES")
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				reply("250" + sep + e)
			}
		case "STARTTLS":
			reply("220 2.0.0 Ready to start TLS")
			tc := tls.Server(raw, f.tlsConfig)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, r, sess.tls = tc, bufio.NewReader(tc), true
		case "AUTH":
			_, ir, _ := strings.Cut(arg, " ")
			if dec, err := base64.StdEncoding.DecodeString(ir); err == nil {
				if parts := strings.Split(string(dec), "\x00"); len(parts) == 3 {
					sess.authUser, sess.authPass = parts[1], parts[2]
				}
			}
			if f.authCode == 235 {
				reply("235 2.7.0 Authentication successful")
			} else {
				reply(fmt.Sprintf("%d 5.7.8 Error: authentication failed", f.authCode))
			}
		case "MAIL":
			sess.mail = arg
			reply("250 2.1.0 Ok")
		case "RCPT":
			rcpt := strings.TrimSuffix(strings.TrimPrefix(arg, "TO:<"), ">")
			code, msg := 250, "2.1.5 Ok"
			if f.rcptReply != nil {
				code, msg = f.rcptReply(rcpt)
			}
			if code == 250 {
				sess.rcpts = append(sess.rcpts, rcpt)
			}
			reply(fmt.Sprintf("%d %s", code, msg))
		case "RSET":
			sess.rcpts = nil
			reply("250 2.0.0 Ok")
		case "DATA":
			reply("354 End data with <CR><LF>.<CR><LF>")
			var data []byte
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data = append(data, strings.TrimPrefix(l, ".")...)
			}
			sess.data, sess.gotData = data, true
			if f.dropAfterData {
				return
			}
			reply(fmt.Sprintf("%d %s", f.dataCode, f.dataMsg))
		case "QUIT":
			reply("221 2.0.0 Bye")
			return
		default:
			reply("502 5.5.2 Error: command not recognized")
		}
	}
}

// selfSignedCert makes a throwaway certificate for name and a pool that
// trusts it.
func selfSignedCert(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
