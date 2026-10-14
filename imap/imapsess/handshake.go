// TLS handshake timeout enforcement for the IMAP listener.
//
// tls.Listen alone performs the handshake lazily on first I/O, which lets a
// client connect and never speak — holding a connection slot, a goroutine,
// and (with enough peers) the whole per-IP budget without ever completing a
// handshake. TLSHandshakeListener accepts from the inner TLS listener,
// completes each handshake eagerly under a deadline in its own goroutine,
// and only surfaces fully-handshaken connections to the IMAP server. Failed
// or timed-out handshakes are closed and counted, never served.
package imapsess

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
	"time"
)

// TLSHandshakeListener wraps a net.Listener whose Accept returns *tls.Conn
// (i.e. tls.NewListener / tls.Listen output) and enforces a handshake
// deadline before a connection is handed to the caller.
type TLSHandshakeListener struct {
	inner   net.Listener
	timeout time.Duration
	logger  *slog.Logger

	conns chan net.Conn
	errs  chan error
	done  chan struct{}
	once  sync.Once
	slots chan struct{}
}

// NewTLSHandshakeListener wraps inner with eager-handshake enforcement.
// A non-positive timeout falls back to 10 seconds — this listener exists
// to bound the handshake, so "unbounded" is never a valid configuration.
func NewTLSHandshakeListener(inner net.Listener, timeout time.Duration, logger *slog.Logger, maxConcurrent ...int) *TLSHandshakeListener {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	bound := 64
	if len(maxConcurrent) > 0 && maxConcurrent[0] > 0 {
		bound = maxConcurrent[0]
	}
	l := &TLSHandshakeListener{
		inner:   inner,
		timeout: timeout,
		logger:  logger,
		conns:   make(chan net.Conn),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
		slots:   make(chan struct{}, bound),
	}
	go l.acceptLoop()
	return l
}

func (l *TLSHandshakeListener) acceptLoop() {
	var delay time.Duration
	for {
		c, err := l.inner.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay = min(2*delay, time.Second)
				}
				l.logger.Warn("IMAP accept failed; retrying", "err", err, "retry_in", delay)
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-l.done:
					timer.Stop()
					return
				}
				continue
			}
			select {
			case l.errs <- err:
			case <-l.done:
			}
			return
		}
		delay = 0
		select {
		case l.slots <- struct{}{}:
			go func() { defer func() { <-l.slots }(); l.handshakeAndDeliver(c) }()
		default:
			metricLimiterRejections.WithLabelValues("tls_handshake_limit").Inc()
			_ = c.Close()
		}
	}
}

func (l *TLSHandshakeListener) handshakeAndDeliver(c net.Conn) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		// Defensive: a non-TLS conn from the inner listener (cleartext dev
		// mode never wraps with this listener) passes through untouched.
		l.deliver(c)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		metricLimiterRejections.WithLabelValues("tls_handshake").Inc()
		l.logger.Warn("TLS handshake failed", "remote", remoteIP(c.RemoteAddr()), "err", err)
		_ = tc.Close()
		return
	}
	l.deliver(tc)
}

func (l *TLSHandshakeListener) deliver(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

// Accept returns the next fully-handshaken connection.
func (l *TLSHandshakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close shuts down the wrapper and the inner listener. Connections whose
// handshake is still in flight are closed when they try to deliver.
func (l *TLSHandshakeListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.inner.Close()
	})
	return err
}

// Addr returns the inner listener's address.
func (l *TLSHandshakeListener) Addr() net.Addr { return l.inner.Addr() }
