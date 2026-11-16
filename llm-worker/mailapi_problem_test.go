package main

import (
	"net/http"
	"testing"
)

func TestPassQueueUnavailableProblemType(t *testing.T) {
	err := &HTTPStatusError{
		Service:     ServiceMailAPI,
		StatusCode:  http.StatusServiceUnavailable,
		ContentType: "application/problem+json",
		ProblemType: "/mail/api/errors/unavailable-pass-queue",
	}
	if !isPassQueueUnavailable(err) {
		t.Error("relative pass-queue problem type was not recognized")
	}
}
