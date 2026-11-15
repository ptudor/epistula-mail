package main

import (
	"net/http"
	"testing"
)

func TestPassQueueUnavailableProblemTypesDuringRollout(t *testing.T) {
	for _, problemType := range []string{
		"/mail/api/errors/unavailable-pass-queue",
		"https://api.ptudor.invalid/errors/unavailable-pass-queue",
	} {
		err := &HTTPStatusError{
			Service:     ServiceMailAPI,
			StatusCode:  http.StatusServiceUnavailable,
			ContentType: "application/problem+json",
			ProblemType: problemType,
		}
		if !isPassQueueUnavailable(err) {
			t.Errorf("problem type %q was not recognized", problemType)
		}
	}
}
