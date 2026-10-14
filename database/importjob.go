package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// checkpointVersion is the shape version of the resume marker. A file without
// one predates job identity (RA6X-031) and is treated as legacy.
const checkpointVersion = 3

// jobIdentity is everything about an import that makes its checkpoint
// meaningful (RA6X-031).
//
// A checkpoint used to record only LastKey, Count and StartedAt, and resume
// compared each key against that unqualified marker. Nothing said which
// Maildir, which database, which mailbox, which folder or which filters
// produced it — so pointing a second import at the same checkpoint path
// SILENTLY SKIPPED every key the first job had already passed, and the
// incomplete result looked exactly like a successful resume. The two
// importers shared the shape, so neither could even tell which had written it.
//
// Identity is compared as a hash, not stored field by field, so the file
// cannot leak a DSN password; the human-readable summary alongside it omits
// credentials for the same reason.
type jobIdentity struct {
	// Kind distinguishes the two importers that share this file shape.
	Kind string `json:"kind"`
	// Source is the canonical absolute path of the tree being read.
	Source string `json:"source"`
	// Destination identifies the database WITHOUT its credentials.
	Destination    string               `json:"destination"`
	DestinationKey string               `json:"destination_key,omitempty"`
	Targets        map[string]jobTarget `json:"targets,omitempty"`
	// Mailbox and Folder are the destination account and folder.
	Mailbox string `json:"mailbox"`
	Folder  string `json:"folder"`
	// Filters are any option that changes WHICH items the job processes, as
	// stable key=value strings.
	Filters []string `json:"filters,omitempty"`
}

// Fingerprint is the comparison value: a hash over every field, so two jobs
// match only if all of them do.
func (j jobIdentity) Fingerprint() string {
	// Store-wide maintenance keys identify files or messages independently
	// of the mailbox catalog. Keep the destination's table identity pinned.
	if j.Kind == "gc-staging" || (j.Kind == "reparse-bodystructure" && j.Mailbox == "") || j.unfilteredBlobImport() {
		j.Targets = nil
	}
	return j.fullFingerprint()
}

// fullFingerprint also recognizes checksums written before store-wide jobs
// stopped including the mailbox catalog. This does not relax job comparison.
func (j jobIdentity) fullFingerprint() string {
	j.Filters = append([]string(nil), j.Filters...)
	sort.Strings(j.Filters)
	data, _ := json.Marshal(j) // struct contains only serializable fields, never credentials
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (j jobIdentity) unfilteredBlobImport() bool {
	return j.Kind == "blob-import" && j.Mailbox == ""
}

// compatible accepts a folder that did not exist when this job was recorded.
// Once a nonzero destination id is recorded it must continue to match.
func (j jobIdentity) compatible(current jobIdentity) bool {
	if !j.unfilteredBlobImport() {
		targets := make(map[string]jobTarget, len(j.Targets))
		for name, old := range j.Targets {
			if now, ok := current.Targets[name]; ok && old.MailboxID == now.MailboxID && old.FolderID == 0 {
				old.FolderID = now.FolderID
			}
			targets[name] = old
		}
		j.Targets = targets
	}
	return j.Fingerprint() == current.Fingerprint()
}

// Keep recorded tenant bindings for validation when that tenant is visited.
// Newly discovered tenants keep the identity just read from the database.
func (j jobIdentity) withRecordedTargets(saved jobIdentity) jobIdentity {
	if j.unfilteredBlobImport() {
		targets := make(map[string]jobTarget, len(j.Targets)+len(saved.Targets))
		for name, target := range j.Targets {
			targets[name] = target
		}
		for name, target := range saved.Targets {
			targets[name] = target
		}
		j.Targets = targets
	}
	return j
}

// String is the operator-facing description used in mismatch diagnostics.
func (j jobIdentity) String() string {
	s := fmt.Sprintf("%s of %s into %s/%s at %s", j.Kind, j.Source, j.Mailbox, j.Folder, j.Destination)
	if len(j.Filters) > 0 {
		s += " [" + strings.Join(j.Filters, " ") + "]"
	}
	return s
}

// canonicalSource resolves a source path to a stable identity: absolute, with
// symlinks resolved, so "./Maildir", "Maildir/" and an absolute path all
// describe the same job.
func canonicalSource(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// errCheckpointMismatch is returned when a checkpoint belongs to a different
// job. Resume must REFUSE rather than skip: skipping is what turned a
// mismatched checkpoint into a silently incomplete import.
var errCheckpointMismatch = errors.New("checkpoint belongs to a different job")

// failedItem is one item a maintenance pass could not process (RA6X-032).
type failedItem struct {
	// Key identifies the item stably enough to retry it: a Maildir entry key,
	// a blob path, a message id.
	Key string `json:"key"`
	// Reason is a short machine-readable classification.
	Reason string `json:"reason"`
	// Detail is the human-readable error.
	Detail string `json:"detail"`
}

// manifestPathFor is the streamed operator report. The adjacent .failures.d
// directory is authoritative and durable before any checkpoint advances.
func manifestPathFor(checkpointPath string) string { return checkpointPath + ".failures.json" }
