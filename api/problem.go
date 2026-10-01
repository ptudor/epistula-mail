package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// RFC 7807 problem+json error writer — the single error shape every
// handler uses, per the project's convention for its HTTP APIs.

const problemTypeBase = "https://api.ptudor.invalid/errors/"

type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// writeProblem emits an RFC 7807 response. slug becomes the type URI's
// final segment; detail is human-facing and must never contain message
// bodies or token material.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, slug, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	p := problem{
		Type:     problemTypeBase + slug,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	}
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.Debug("problem encode", "err", err)
	}
}

func problemBadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusBadRequest, "bad-request", "Bad Request", detail)
}

func problemUnauthorized(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="epistula-api"`)
	writeProblem(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", detail)
}

func problemForbidden(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusForbidden, "forbidden", "Forbidden", detail)
}

func problemNotFound(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusNotFound, "not-found", "Not Found", detail)
}

func problemUnprocessable(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusUnprocessableEntity, "invalid-parameter", "Unprocessable Entity", detail)
}

func problemTooMany(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusTooManyRequests, "rate-limited", "Too Many Requests", detail)
}

func problemInternal(w http.ResponseWriter, r *http.Request) {
	// Deliberately detail-free: internals go to the log, not the client.
	writeProblem(w, r, http.StatusInternalServerError, "internal", "Internal Server Error", "")
}

func problemUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusServiceUnavailable, "unavailable", "Service Unavailable", detail)
}

// problemMethodNotAllowed emits a 405 with the Allow header set to the methods
// the matched path accepts (R-068). The caller must have already set Allow, or
// pass it here.
func problemMethodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed", "Method Not Allowed",
		"That method is not allowed on this endpoint.")
}
