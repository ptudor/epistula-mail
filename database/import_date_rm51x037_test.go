package main

import (
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maildir"
	"testing"
	"time"
)

func TestInternalDatePrefersLastHopAndBoundsSenderDate(t *testing.T) {
	old := time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC)
	msg := &ingest.Message{SentDate: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), Headers: map[string][]string{"Received": {"by final; Fri, 01 Jan 2010 00:00:00 +0000", "by older; Thu, 31 Dec 2009 00:00:00 +0000"}}}
	if got := chooseInternalDate(msg, maildir.Entry{ModTime: old}); got.Year() != 2010 {
		t.Fatal(got)
	}
	msg.Headers = nil
	if got := chooseInternalDate(msg, maildir.Entry{ModTime: old}); !got.Equal(old) {
		t.Fatal(got)
	}
	msg.SentDate = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := chooseInternalDate(msg, maildir.Entry{ModTime: old}); !got.Equal(old) {
		t.Fatal(got)
	}
}
func TestDomainAcceptsAndStoresIDNA(t *testing.T) {
	if err := validateDomainName("café.example"); err != nil {
		t.Fatal(err)
	}
	if got := normalizeDomain("café.example"); got != "xn--caf-dma.example" {
		t.Fatal(got)
	}
}
