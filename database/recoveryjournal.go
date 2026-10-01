package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The directory is the authoritative failure manifest, including after a
// crash. One atomically published, synced record per key gives O(1) lookup
// and bounded memory without dropping failures. The JSON report is a streamed
// snapshot for operators. A directory lock excludes concurrent recovery jobs.
//
// A dry run writes nothing, so it keeps in memory the difference between the
// queue on disk and the queue the same real run would leave: the records it
// would remove and the keys it would add (OPS-005). contains, each and the
// count then answer as that real run's queue would, so a dry run reports what
// the real run would leave unresolved. The overlay is bounded by the records on
// disk plus the new failures the pass finds.
type recoveryJournal struct {
	dir, report string
	job         jobIdentity
	fd          *os.File
	dry, retry  bool
	count       int64
	// queued is false only for a dry run that found no queue on disk. It has
	// no records to read, and a dry run creates nothing.
	queued bool
	// dryRemoved holds the keys of records on disk that the real run would
	// have removed; dryAdded holds the keys it would have added.
	dryRemoved, dryAdded map[string]struct{}
}

func openRecovery(path string, job jobIdentity, dry, retry bool) (*recoveryJournal, error) {
	j := &recoveryJournal{dir: path + ".failures.d", report: manifestPathFor(path), job: job, dry: dry, retry: retry, queued: true}
	if path == "" {
		return nil, fmt.Errorf("a checkpoint/manifest path is required")
	}
	if dry {
		j.dryRemoved, j.dryAdded = map[string]struct{}{}, map[string]struct{}{}
	}
	_, err := os.Stat(j.dir)
	if errors.Is(err, os.ErrNotExist) {
		if retry {
			return nil, fmt.Errorf("no durable failure queue at %s; use the original job/checkpoint, or run a complete scan to migrate an older report", j.dir)
		}
		if dry {
			j.queued = false
			return j, nil
		}
		if _, err := os.Stat(j.report); err == nil {
			return nil, fmt.Errorf("legacy failure report %s exists; retain it and use a new checkpoint path for a complete deduplicating scan", j.report)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := os.Mkdir(j.dir, 0700); err != nil {
			return nil, err
		}
		if err := syncParent(j.dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	j.fd, err = os.Open(j.dir)
	if err != nil {
		return nil, err
	}
	lock := syscall.LOCK_EX | syscall.LOCK_NB
	if dry {
		lock = syscall.LOCK_SH | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(j.fd.Fd()), lock); err != nil {
		j.Close()
		return nil, fmt.Errorf("recovery queue is in use: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(j.dir, "job.json"))
	if errors.Is(err, os.ErrNotExist) && !dry && !retry {
		// A crash immediately after mkdir is safe only if no records exist.
		if err = j.each(func(failedItem) error { return fmt.Errorf("queue has records without identity") }); err == nil {
			err = j.bind(job)
		}
	} else if err == nil {
		var saved jobIdentity
		if err = json.Unmarshal(data, &saved); err == nil && saved.Fingerprint() != job.Fingerprint() {
			err = fmt.Errorf("%w: failure queue %s; use a new checkpoint for a changed job", errCheckpointMismatch, j.dir)
		}
	}
	if err != nil {
		j.Close()
		return nil, err
	}
	err = j.each(func(failedItem) error { j.count++; return nil })
	if err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

func (j *recoveryJournal) Close() {
	if j.fd != nil {
		j.fd.Close()
		j.fd = nil
	}
}
func (j *recoveryJournal) bind(job jobIdentity) error {
	if j.dry {
		j.job = job
		return nil
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err = writeDurableFile(filepath.Join(j.dir, "job.json"), data); err != nil {
		return err
	}
	j.job = job
	return nil
}
func (j *recoveryJournal) itemPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(j.dir, hex.EncodeToString(sum[:])+".json")
}

// contains reports whether key has a record, as the real run's queue would at
// this point in the pass.
func (j *recoveryJournal) contains(key string) (bool, error) {
	if j.dry {
		if _, added := j.dryAdded[key]; added {
			return true, nil
		}
		if _, removed := j.dryRemoved[key]; removed {
			return false, nil
		}
	}
	if !j.queued {
		return false, nil
	}
	data, err := os.ReadFile(j.itemPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var item failedItem
	if err = json.Unmarshal(data, &item); err != nil {
		return false, err
	}
	if item.Key != key {
		return false, fmt.Errorf("failure key mismatch")
	}
	return true, nil
}
func (j *recoveryJournal) selected(key string) (bool, error) {
	if !j.retry {
		return true, nil
	}
	return j.contains(key)
}
func (j *recoveryJournal) fail(key, reason string, cause error) error {
	exists, err := j.contains(key)
	if err != nil {
		return err
	}
	if j.dry {
		// A dry run used to count every failure as new, so a key already in
		// the queue was counted twice (OPS-005).
		if !exists {
			if _, removed := j.dryRemoved[key]; removed {
				delete(j.dryRemoved, key)
			} else {
				j.dryAdded[key] = struct{}{}
			}
			j.count++
		}
		return nil
	}
	item := failedItem{Key: key, Reason: reason}
	if cause != nil {
		item.Detail = cause.Error()
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err = writeDurableFile(j.itemPath(key), data); err != nil {
		return err
	}
	if !exists {
		j.count++
	}
	return nil
}
func (j *recoveryJournal) resolved(key string) error {
	if j.dry {
		return j.dryResolved(key)
	}
	if err := os.Remove(j.itemPath(key)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := j.fd.Sync(); err != nil {
		return err
	}
	j.count--
	return nil
}

// dryResolved is resolved for a dry run. The real run removes the record if
// there is one, whatever it holds, so the record's existence alone decides.
// A dry run used to leave the count alone here, so every record the pass
// would clear was still reported unresolved (OPS-005).
func (j *recoveryJournal) dryResolved(key string) error {
	if _, added := j.dryAdded[key]; added {
		delete(j.dryAdded, key)
		j.count--
		return nil
	}
	if _, removed := j.dryRemoved[key]; removed || !j.queued {
		return nil
	}
	if _, err := os.Lstat(j.itemPath(key)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	j.dryRemoved[key] = struct{}{}
	j.count--
	return nil
}

// ReadDir(n) keeps directory enumeration bounded too. Temporary staging files
// are never authoritative and cannot be mistaken for failed items.
//
// A dry run visits what the real run's queue would hold: the records on disk
// it has not removed, then the keys it has added. An added key carries only
// the key, since only the queue on disk keeps a reason.
func (j *recoveryJournal) each(visit func(failedItem) error) error {
	if j.queued {
		if err := j.eachOnDisk(visit); err != nil {
			return err
		}
	}
	if !j.dry {
		return nil
	}
	for key := range j.dryAdded {
		if err := visit(failedItem{Key: key}); err != nil {
			return err
		}
	}
	return nil
}

func (j *recoveryJournal) eachOnDisk(visit func(failedItem) error) error {
	f, err := os.Open(j.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		if err != nil && err != io.EOF {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if len(name) != 69 || !strings.HasSuffix(name, ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(j.dir, name))
			if err != nil {
				return err
			}
			var item failedItem
			if err = json.Unmarshal(data, &item); err != nil {
				return err
			}
			if filepath.Base(j.itemPath(item.Key)) != name {
				return fmt.Errorf("invalid failure record %s", name)
			}
			if _, removed := j.dryRemoved[item.Key]; removed {
				continue
			}
			if err = visit(item); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}
func (j *recoveryJournal) finish() (int64, error) {
	if j.dry {
		return j.count, nil
	}
	if j.count == 0 {
		if err := os.Remove(j.report); err == nil {
			return 0, syncParent(j.report)
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, nil
	}
	err := writeDurableStream(j.report, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		if _, err := io.WriteString(w, "{\"job\":"); err != nil {
			return err
		}
		if err := enc.Encode(j.job); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, ",\"written\":%q,\"items\":[", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		first := true
		if err := j.each(func(item failedItem) error {
			if !first {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			first = false
			return enc.Encode(item)
		}); err != nil {
			return err
		}
		_, err := io.WriteString(w, "]}\n")
		return err
	})
	return j.count, err
}

func syncParent(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
