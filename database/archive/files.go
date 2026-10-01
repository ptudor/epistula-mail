// Package archive implements archive sorting: filing mail into an
// operator-approved category tree under a mailbox's \Archive folder, from the
// classifications epistula-llm-worker writes through epistula-api
// (ARCHIVE_SORTING.md at the repository root).
//
// It has three consumers. `epistula-database admin` uses the taxonomy and rules
// files and the plan/apply/undo cycle for the one-time reorganization of an
// imported archive. epistula-imap runs SortOnce and PurgeOnce on a timer: the
// live sorter that files what a user archives, and the Trash purge that
// destroys what a user deletes. All three move and destroy mail only through
// storage.MoveMessages and storage.PurgeMessages.
package archive

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/ptudor/epistula-mail/database/storage"
)

// taxonomyFile is the on-disk shape of a mailbox's category list:
//
//	[[category]]
//	key         = "finance/banking/acme-bank"
//	folder      = "Archive/Finance/Banking/Acme Bank"
//	description = "Acme Bank statements, card alerts, transfers"
//	annual      = true   # file into .../Acme Bank/<YYYY>
type taxonomyFile struct {
	Category []struct {
		Key         string `toml:"key"`
		Folder      string `toml:"folder"`
		Description string `toml:"description"`
		Annual      bool   `toml:"annual"`
	} `toml:"category"`
}

// ParseTaxonomy reads a category list. Structural problems are reported here;
// the rules that depend on the mailbox (every folder inside its \Archive
// folder, none a special-use folder) are checked by
// storage.ReplaceArchiveCategories under the mailbox lock.
func ParseTaxonomy(r io.Reader) ([]storage.ArchiveCategory, error) {
	var f taxonomyFile
	dec := toml.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse category list: %w", err)
	}
	if len(f.Category) == 0 {
		return nil, errors.New("parse category list: no [[category]] entries")
	}
	out := make([]storage.ArchiveCategory, 0, len(f.Category))
	for i, c := range f.Category {
		key := strings.TrimSpace(c.Key)
		folder := strings.TrimSpace(c.Folder)
		if key == "" || folder == "" {
			return nil, fmt.Errorf("parse category list: entry %d needs both key and folder", i+1)
		}
		out = append(out, storage.ArchiveCategory{
			Key:         key,
			Folder:      folder,
			Description: strings.TrimSpace(c.Description),
			Annual:      c.Annual,
		})
	}
	return out, nil
}

// Rules steer the one-time reorganization (BuildPlan). The live sorter does
// not use them: it only ever files what a user put in the \Archive folder.
type Rules struct {
	// MinConfidence is the classifier confidence below which a message is not
	// filed by its classification.
	MinConfidence float64 `toml:"min_confidence"`
	// InboxKeepDays keeps INBOX messages that arrived within this many days
	// where they are. Older INBOX mail is drained into the archive.
	InboxKeepDays int `toml:"inbox_keep_days"`
	// Keep lists folders left exactly as they are. An entry matches the folder
	// of that name and every folder below it.
	Keep []string `toml:"keep"`
	// Drain lists folders to empty: a message there that cannot be filed by
	// its classification goes to the \Archive folder itself instead of
	// staying, where the live sorter files it once it is classified. Matches
	// like Keep.
	Drain []string `toml:"drain"`
	// Folders files a whole folder (and everything below it) into one
	// category key without consulting the classifier. The longest matching
	// entry wins.
	Folders map[string]string `toml:"folders"`
}

// DefaultRules is the policy used when no rules file is given.
func DefaultRules() Rules {
	return Rules{MinConfidence: 0.6, InboxKeepDays: 30}
}

// ParseRules reads a rules file over DefaultRules, so an omitted setting keeps
// its default.
func ParseRules(r io.Reader) (Rules, error) {
	rules := DefaultRules()
	dec := toml.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rules); err != nil {
		return Rules{}, fmt.Errorf("parse rules: %w", err)
	}
	if err := rules.Validate(); err != nil {
		return Rules{}, err
	}
	return rules, nil
}

// Validate checks the settings that do not depend on a mailbox.
func (r Rules) Validate() error {
	if math.IsNaN(r.MinConfidence) || r.MinConfidence < 0 || r.MinConfidence > 1 {
		return fmt.Errorf("rules: min_confidence must be between 0 and 1, got %v", r.MinConfidence)
	}
	if r.InboxKeepDays < 0 {
		return fmt.Errorf("rules: inbox_keep_days must not be negative, got %d", r.InboxKeepDays)
	}
	for _, list := range [][]string{r.Keep, r.Drain} {
		for _, name := range list {
			if err := storage.ValidateFolderName(name); err != nil {
				return fmt.Errorf("rules: %w", err)
			}
		}
	}
	for name, key := range r.Folders {
		if err := storage.ValidateFolderName(name); err != nil {
			return fmt.Errorf("rules: folders: %w", err)
		}
		if !storage.ValidArchiveKey(key) {
			return fmt.Errorf("rules: folders: %q maps to %q, which is not a category key", name, key)
		}
	}
	return nil
}

// matchesSubtree reports whether folder is name or lies below it.
func matchesSubtree(folder, name string) bool {
	return folder == name || strings.HasPrefix(folder, name+"/")
}

// longestSubtreeMatch returns the entry of m that matches folder most
// specifically, and whether any did.
func longestSubtreeMatch(folder string, m map[string]string) (string, bool) {
	best, found := "", false
	for name := range m {
		if matchesSubtree(folder, name) && (!found || len(name) > len(best)) {
			best, found = name, true
		}
	}
	if !found {
		return "", false
	}
	return m[best], true
}

// DestinationFolder is where a category files a message: the category's
// folder, or for an annual category the year folder below it,
// <folder>/<YYYY>. The reorganization and the live sorter both call it, so a
// message lands in the same place whichever files it.
//
// The year is the sender's own calendar date (sent_date_local, which keeps the
// Date header's offset) and otherwise the arrival date in UTC — never the
// model's opinion. A year before 1971 or after next year is taken as a broken
// Date header rather than a real one, and such a message files into the
// category folder itself instead of a year folder nobody would look in.
func DestinationFolder(folder string, annual bool, sentLocal *time.Time, internal, now time.Time) string {
	if !annual {
		return folder
	}
	plausible := func(y int) bool { return y >= 1971 && y <= now.Year()+1 }
	if sentLocal != nil && plausible(sentLocal.Year()) {
		return fmt.Sprintf("%s/%04d", folder, sentLocal.Year())
	}
	if y := internal.UTC().Year(); plausible(y) {
		return fmt.Sprintf("%s/%04d", folder, y)
	}
	return folder
}
