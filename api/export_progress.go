package main

import (
	"io"
	"net/http"
	"time"
)

// Bound each write/flush to a small progress unit, including within one large
// JSON row. A per-row deadline incorrectly times out a healthy slow transfer.
// FlushError observes transport failures hidden by http.Flusher.Flush.
type exportProgressWriter struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	stall time.Duration
}

func (p *exportProgressWriter) refresh() error {
	if p.stall <= 0 {
		return nil
	}
	return p.rc.SetWriteDeadline(time.Now().Add(p.stall))
}
func (p *exportProgressWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		if err := p.refresh(); err != nil {
			return written, err
		}
		size := min(len(b), 8*1024)
		n, err := p.w.Write(b[:size])
		written += n
		if err != nil {
			return written, err
		}
		if n != size {
			return written, io.ErrShortWrite
		}
		if err := p.rc.Flush(); err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}
func (p *exportProgressWriter) Flush() error {
	if err := p.refresh(); err != nil {
		return err
	}
	return p.rc.Flush()
}
