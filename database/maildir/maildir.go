// Package maildir walks a Maildir directory tree and translates its
// per-file flag suffixes into the IMAP flag set epistula-database stores. See
// http://cr.yp.to/proto/maildir.html for the format.
package maildir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Flags captures the parsed Maildir flag set from a `:2,XYZ` filename suffix.
// Maildir defines six flags; only D / F / R / S / T have direct IMAP analogs.
type Flags struct {
	Draft    bool // D
	Flagged  bool // F
	Passed   bool // P → emitted as a $Forwarded keyword
	Answered bool // R
	Seen     bool // S
	Trashed  bool // T → IMAP \Deleted
}

// ParseFlags reads the `:2,XYZ` suffix off a Maildir filename. Returns the
// zero value (no flags) when the suffix is absent.
func ParseFlags(filename string) Flags {
	idx := strings.LastIndex(filename, ":2,")
	if idx < 0 {
		return Flags{}
	}
	var f Flags
	for _, c := range filename[idx+3:] {
		switch c {
		case 'D':
			f.Draft = true
		case 'F':
			f.Flagged = true
		case 'P':
			f.Passed = true
		case 'R':
			f.Answered = true
		case 'S':
			f.Seen = true
		case 'T':
			f.Trashed = true
		}
	}
	return f
}

// IMAP returns the IMAP4-style flag strings for the parsed set.
// `\Recent` is excluded — it is per-session and not stored.
func (f Flags) IMAP() []string {
	out := make([]string, 0, 5)
	if f.Draft {
		out = append(out, `\Draft`)
	}
	if f.Flagged {
		out = append(out, `\Flagged`)
	}
	if f.Answered {
		out = append(out, `\Answered`)
	}
	if f.Seen {
		out = append(out, `\Seen`)
	}
	if f.Trashed {
		out = append(out, `\Deleted`)
	}
	if f.Passed {
		out = append(out, "$Forwarded")
	}
	return out
}

// Entry is one Maildir message file.
type Entry struct {
	Path     string    // absolute path
	BaseName string    // filename without directory
	Flags    Flags     // parsed from BaseName
	ModTime  time.Time // file mtime (used as INTERNALDATE fallback)
	Size     int64
	Sub      string // "cur" or "new"
}

// Key returns the stable, root-independent identity of the entry:
// "<sub>/<basename>". Walk emits entries in strictly ascending Key order,
// which is what makes a "skip everything <= checkpoint key" resume
// predicate sound — independent of how the caller spelled the Maildir root
// (trailing slash, relative vs. absolute, symlink).
func (e Entry) Key() string { return e.Sub + "/" + e.BaseName }

// Walk yields every message file under root's cur/ and new/ subdirectories
// in ascending Entry.Key order (one global sort across both subdirs — the
// ordering is guaranteed, not an accident of directory iteration). tmp/ is
// skipped (incomplete writes). visit may return ErrStop to halt the walk
// without surfacing an error.
type VisitFunc func(Entry) error

// ErrStop terminates Walk without an error.
var ErrStop = errors.New("maildir: stop")

func Walk(root string, visit VisitFunc) error {
	if root == "" {
		return errors.New("maildir: empty root")
	}
	if info, err := os.Stat(root); err != nil {
		return fmt.Errorf("stat %s: %w", root, err)
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	var all []Entry
	for _, sub := range []string{"cur", "new"} {
		dir := filepath.Join(root, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return fmt.Errorf("stat %s/%s: %w", dir, e.Name(), err)
			}
			all = append(all, Entry{
				Path:     filepath.Join(dir, e.Name()),
				BaseName: e.Name(),
				Flags:    ParseFlags(e.Name()),
				ModTime:  info.ModTime(),
				Size:     info.Size(),
				Sub:      sub,
			})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
	for _, ent := range all {
		err := visit(ent)
		if errors.Is(err, ErrStop) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
