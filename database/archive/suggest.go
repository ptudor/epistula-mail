package archive

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"

	"github.com/ptudor/epistula-mail/database/storage"
)

// The adaptive-taxonomy report (ARCHIVE_SORTING.md, "Growing the category
// list"). It looks at where the classifier is unsure — the …/other buckets,
// and anything classified below min_confidence — and finds the senders and the
// themes that recur there, proposing a sibling category for each. It proposes
// only: a category exists when the owner puts it in the list and re-imports
// it. The model never creates one. Mail is attacker-controlled text, and a
// category list the mail could extend would let a stranger decide where the
// owner's mail goes.

// SuggestOptions configures Suggest.
type SuggestOptions struct {
	// MinCount and MinShare decide which clusters are reported: at least
	// MinCount messages, or at least MinShare (0..1) of their bucket.
	MinCount int
	MinShare float64
	// Examples is how many example subjects each cluster shows.
	Examples int
	// MinConfidence is the confidence below which a classification counts as
	// unsure, as the sorter's min_confidence does.
	MinConfidence float64
	// MaxTagShare drops a tag as a theme when more than this share (0..1) of
	// the mailbox's annotations carry it: "newsletter" or "promotional" say
	// what kind of mail it is, not what it is about. 1 keeps every tag.
	MaxTagShare float64
	// SenderOverlap drops a tag theme when at least this share (0..1) of its
	// messages are also one sender cluster of the same bucket: the tag
	// "tidepool" is the sender tidepool.example again.
	SenderOverlap float64
}

// DefaultSuggestOptions are the report's defaults.
func DefaultSuggestOptions() SuggestOptions {
	return SuggestOptions{MinCount: 20, MinShare: 0.05, Examples: 3, MinConfidence: 0.6,
		MaxTagShare: 0.03, SenderOverlap: 0.8}
}

// Proposal is one suggested new category.
type Proposal struct {
	Key    string
	Folder string
	Annual bool
	// Conflict names why the proposal cannot be imported as it stands: an
	// existing key or folder, or no usable slug. Empty when it can.
	Conflict string
}

// Cluster is a recurring sender or theme inside a bucket.
type Cluster struct {
	// Label is the sender organisation (registrable domain) or the tag.
	Label    string
	Count    int
	Share    float64
	TopTags  []string
	Examples []string
	Proposal Proposal
}

// Bucket is one place the classifier was unsure.
type Bucket struct {
	Key    string
	Folder string
	// LowConfidence is true for a bucket of messages classified into a
	// specific key below MinConfidence; false for an …/other key.
	LowConfidence bool
	Count         int
	BySender      []Cluster
	ByTag         []Cluster
}

// SuggestReport is the whole report for one mailbox.
type SuggestReport struct {
	Options SuggestOptions
	Buckets []Bucket
}

// IsOtherKey reports whether key is an "other" bucket: "other" itself or any
// key whose last segment is "other".
func IsOtherKey(key string) bool {
	return key == "other" || strings.HasSuffix(key, "/other")
}

// Suggest builds the report. It only reads, and takes no lock.
func Suggest(ctx context.Context, db *storage.DB, mailboxID int64, opts SuggestOptions) (*SuggestReport, error) {
	if opts.MinCount < 1 || opts.MinShare < 0 || opts.MinShare > 1 || opts.Examples < 0 ||
		opts.MinConfidence < 0 || opts.MinConfidence > 1 ||
		opts.MaxTagShare < 0 || opts.MaxTagShare > 1 || opts.SenderOverlap < 0 || opts.SenderOverlap > 1 {
		return nil, fmt.Errorf("archive: suggest options out of range: %+v", opts)
	}
	cats := map[string]catInfo{}
	folders := map[string]bool{}
	rows, err := db.Pool().Query(ctx,
		`SELECT key, folder, annual, retired_at IS NULL FROM archive_categories WHERE mailbox_id = $1`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}
	var otherKeys []string
	for rows.Next() {
		var key string
		var c catInfo
		if err := rows.Scan(&key, &c.folder, &c.annual, &c.active); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan category: %w", err)
		}
		cats[key] = c
		folders[c.folder] = true
		if c.active && IsOtherKey(key) {
			otherKeys = append(otherKeys, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}
	root, err := storage.FindSpecialUseFolder(ctx, db.Pool(), mailboxID, `\Archive`)
	if err != nil {
		return nil, fmt.Errorf("archive folder: %w", err)
	}

	type msg struct {
		id       int64
		from     string
		subject  string
		internal time.Time
		tags     []string
	}
	buckets := map[string][]msg{}
	rows, err = db.Pool().Query(ctx, `
		SELECT c.category, c.confidence, c.model, m.id, COALESCE(m.from_addr, ''), COALESCE(m.subject, ''),
		       m.internal_date, COALESCE(a.tags, '{}')
		  FROM message_classifications c
		  JOIN messages m ON m.id = c.message_id
		  JOIN folders f ON f.id = m.folder_id
		  JOIN archive_categories ac
		    ON ac.mailbox_id = f.mailbox_id AND ac.key = c.category AND ac.retired_at IS NULL
		  LEFT JOIN message_annotations a ON a.message_id = m.id AND a.model = c.model
		 WHERE f.mailbox_id = $1
		   AND (c.category = ANY($2) OR c.confidence < $3)`,
		mailboxID, otherKeys, opts.MinConfidence)
	if err != nil {
		return nil, fmt.Errorf("read classifications: %w", err)
	}
	models := map[string]bool{}
	for rows.Next() {
		var key, model string
		var conf float32
		var m msg
		if err := rows.Scan(&key, &conf, &model, &m.id, &m.from, &m.subject, &m.internal, &m.tags); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan classification: %w", err)
		}
		models[model] = true
		bucket := key
		if !IsOtherKey(key) {
			bucket = key + lowConfidenceSuffix
		}
		buckets[bucket] = append(buckets[bucket], m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read classifications: %w", err)
	}

	generic, err := genericTags(ctx, db, mailboxID, models, opts.MaxTagShare)
	if err != nil {
		return nil, err
	}

	report := &SuggestReport{Options: opts}
	names := make([]string, 0, len(buckets))
	for b := range buckets {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, name := range names {
		msgs := buckets[name]
		key, low := strings.CutSuffix(name, lowConfidenceSuffix)
		info := cats[key]
		b := Bucket{Key: key, Folder: info.folder, LowConfidence: low, Count: len(msgs)}
		parentKey, parentFolder := proposalParent(key, info.folder, root.Name)

		// Group by sender organisation, and by tag.
		bySender := map[string][]msg{}
		byTag := map[string][]msg{}
		for _, m := range msgs {
			org := SenderOrganization(m.from)
			bySender[org] = append(bySender[org], m)
			seen := map[string]bool{}
			for _, t := range m.tags {
				t = strings.ToLower(strings.TrimSpace(t))
				if t == "" || seen[t] {
					continue
				}
				seen[t] = true
				byTag[t] = append(byTag[t], m)
			}
		}
		qualifies := func(n int) bool {
			return n >= opts.MinCount || (len(msgs) > 0 && float64(n)/float64(len(msgs)) >= opts.MinShare && n > 1)
		}
		build := func(label string, group []msg, slugSource string) Cluster {
			sort.Slice(group, func(i, j int) bool { return group[i].internal.After(group[j].internal) })
			c := Cluster{Label: label, Count: len(group), Share: float64(len(group)) / float64(len(msgs))}
			tagCount := map[string]int{}
			for _, m := range group {
				for _, t := range m.tags {
					if t = strings.ToLower(strings.TrimSpace(t)); t != "" && t != label {
						tagCount[t]++
					}
				}
			}
			for _, kv := range sortedCounts(tagCount) {
				if len(c.TopTags) == 5 {
					break
				}
				c.TopTags = append(c.TopTags, kv.k)
			}
			for _, m := range group {
				if len(c.Examples) == opts.Examples {
					break
				}
				c.Examples = append(c.Examples, SafeLine(m.subject, 120))
			}
			c.Proposal = propose(parentKey, parentFolder, slugSource, info.annual, cats, folders)
			return c
		}
		for org, group := range bySender {
			if org != "" && qualifies(len(group)) {
				b.BySender = append(b.BySender, build(org, group, orgLabel(org)))
			}
		}
		senderOf := map[int64]string{}
		for org, group := range bySender {
			for _, m := range group {
				senderOf[m.id] = org
			}
		}
		for tag, group := range byTag {
			if generic[tag] || !qualifies(len(group)) {
				continue
			}
			// A theme that is one sender under another name adds nothing
			// the sender cluster does not already say.
			perSender := map[string]int{}
			top := 0
			for _, m := range group {
				if org := senderOf[m.id]; org != "" {
					perSender[org]++
					top = max(top, perSender[org])
				}
			}
			if float64(top) >= opts.SenderOverlap*float64(len(group)) && opts.SenderOverlap < 1 {
				continue
			}
			b.ByTag = append(b.ByTag, build(tag, group, tag))
		}
		for _, list := range [][]Cluster{b.BySender, b.ByTag} {
			sort.Slice(list, func(i, j int) bool {
				if list[i].Count != list[j].Count {
					return list[i].Count > list[j].Count
				}
				return list[i].Label < list[j].Label
			})
		}
		report.Buckets = append(report.Buckets, b)
	}
	return report, nil
}

const lowConfidenceSuffix = "\x00low"

// proposalParent is where a bucket's proposals go: beside the bucket's key.
// "a/b/other" proposes "a/b/<new>" in the folder above Other's; a
// low-confidence "a/b/c" proposes "a/b/<new>" too, a sibling of the key the
// model reached for.
func proposalParent(key, folder, root string) (string, string) {
	parentKey := ""
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		parentKey = key[:i]
	}
	parentFolder := root
	if i := strings.LastIndexByte(folder, '/'); i >= 0 && storage.IsUnderFolder(folder, root) {
		parentFolder = folder[:i]
	}
	return parentKey, parentFolder
}

// genericTags returns the tags carried by more than maxShare of the mailbox's
// annotations by the given models: labels of the kind of mail, which every
// bucket shares, rather than of its subject.
func genericTags(ctx context.Context, db *storage.DB, mailboxID int64, models map[string]bool, maxShare float64) (map[string]bool, error) {
	out := map[string]bool{}
	if len(models) == 0 || maxShare >= 1 {
		return out, nil
	}
	list := make([]string, 0, len(models))
	for m := range models {
		list = append(list, m)
	}
	var total int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM message_annotations a
		  JOIN messages m ON m.id = a.message_id JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1 AND a.model = ANY($2)`, mailboxID, list,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("count annotations: %w", err)
	}
	if total == 0 {
		return out, nil
	}
	rows, err := db.Pool().Query(ctx, `
		SELECT norm.tag, count(DISTINCT a.message_id)
		  FROM message_annotations a
		  JOIN messages m ON m.id = a.message_id JOIN folders f ON f.id = m.folder_id,
		       unnest(a.tags) AS raw(tag), lower(btrim(raw.tag)) AS norm(tag)
		 WHERE f.mailbox_id = $1 AND a.model = ANY($2)
		 GROUP BY norm.tag
		HAVING count(DISTINCT a.message_id) > $3`, mailboxID, list, maxShare*float64(total))
	if err != nil {
		return nil, fmt.Errorf("tag frequencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var n int64
		if err := rows.Scan(&tag, &n); err != nil {
			return nil, fmt.Errorf("scan tag frequency: %w", err)
		}
		out[tag] = true
	}
	return out, rows.Err()
}

// catInfo is what the report needs to know about one category.
type catInfo struct {
	folder string
	annual bool
	active bool
}

// propose names a new category beside the bucket's key for a sender label or
// a tag, and says why it cannot be imported as it stands, if it cannot.
func propose(parentKey, parentFolder, source string, annual bool, cats map[string]catInfo, folders map[string]bool) Proposal {
	slug := Slug(source)
	if slug == "" {
		return Proposal{Conflict: "no usable key"}
	}
	key := slug
	if parentKey != "" {
		key = parentKey + "/" + slug
	}
	p := Proposal{Key: key, Folder: parentFolder + "/" + folderLabel(source), Annual: annual}
	switch {
	case !storage.ValidArchiveKey(key):
		p.Conflict = "not a valid key"
	case storage.ValidateFolderName(p.Folder) != nil:
		p.Conflict = "not a valid folder name"
	default:
		if c, ok := cats[key]; ok {
			if c.active {
				p.Conflict = "key exists"
			} else {
				p.Conflict = "key exists, retired"
			}
		} else if folders[p.Folder] {
			p.Conflict = "folder used by another key"
		}
	}
	return p
}

// SenderOrganization reduces a From address to the organisation that sent it:
// its registrable domain (the public suffix plus one label), so
// alerts.initech.example and initech.example are one sender. An address with
// no usable domain yields "".
func SenderOrganization(from string) string {
	at := strings.LastIndexByte(from, '@')
	if at < 0 {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(strings.Trim(from[at+1:], "<>\"' \t")))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" || strings.ContainsAny(domain, " []:") {
		return ""
	}
	if org, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return org
	}
	return domain
}

// orgLabel is the part of a registrable domain an owner would call the
// sender: acmeshop.example gives acmeshop, example.co.uk gives example.
func orgLabel(org string) string {
	suffix, _ := publicsuffix.PublicSuffix(org)
	if label := strings.TrimSuffix(org, "."+suffix); label != "" && label != org {
		return label
	}
	return org
}

// Slug makes a category key segment of s: lower-case letters, digits and
// single hyphens, starting with a letter or digit, at most 40 characters.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	return out
}

// folderLabel is how a new folder is named from a sender label or a tag: the
// words capitalised, hyphens and dots as spaces.
func folderLabel(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' || unicode.IsSpace(r) })
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	return SafeLine(strings.Join(words, " "), 60)
}

// SafeLine makes sender-controlled text fit to print on one line of a
// terminal or a TOML comment: control characters (including the escape that
// starts a terminal control sequence) become spaces, and the text is cut to
// max runes. Subjects and tags come from mail, which anyone can send.
func SafeLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "..."
	}
	return s
}

// WriteText prints the report for a human.
func (r *SuggestReport) WriteText(w io.Writer) error {
	o := r.Options
	if _, err := fmt.Fprintf(w, "%d bucket(s); clusters of at least %d messages or %.0f%% of their bucket; unsure below confidence %.2f\n",
		len(r.Buckets), o.MinCount, o.MinShare*100, o.MinConfidence); err != nil {
		return err
	}
	for _, b := range r.Buckets {
		kind := "other bucket"
		if b.LowConfidence {
			kind = "classified below the confidence threshold"
		}
		if _, err := fmt.Fprintf(w, "\n%s (%s): %d messages\n", b.Key, kind, b.Count); err != nil {
			return err
		}
		for _, sec := range []struct {
			title string
			list  []Cluster
		}{{"by sender", b.BySender}, {"by tag", b.ByTag}} {
			if len(sec.list) == 0 {
				continue
			}
			if _, err := fmt.Fprintf(w, "  %s:\n", sec.title); err != nil {
				return err
			}
			for _, c := range sec.list {
				prop := c.Proposal.Key
				if prop != "" {
					prop = fmt.Sprintf("→ %s  %q", c.Proposal.Key, c.Proposal.Folder)
				}
				if c.Proposal.Conflict != "" {
					prop += "  [" + c.Proposal.Conflict + "]"
				}
				if _, err := fmt.Fprintf(w, "    %-28s %5d  %3.0f%%  %s\n", SafeLine(c.Label, 28), c.Count, c.Share*100, prop); err != nil {
					return err
				}
				if len(c.TopTags) > 0 {
					if _, err := fmt.Fprintf(w, "        tags: %s\n", SafeLine(strings.Join(c.TopTags, ", "), 100)); err != nil {
						return err
					}
				}
				for _, ex := range c.Examples {
					if _, err := fmt.Fprintf(w, "        e.g. %s\n", ex); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// WriteTOML prints the importable proposals as commented-out [[category]]
// stanzas, to review and paste into the category list. A proposal with a
// conflict is listed as a comment only.
func (r *SuggestReport) WriteTOML(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "# Proposed archive categories. Nothing here is active: uncomment what you want,\n# give it a description, add it to the category list and re-import it."); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, b := range r.Buckets {
		for _, c := range append(append([]Cluster{}, b.BySender...), b.ByTag...) {
			p := c.Proposal
			if p.Key != "" && seen[p.Key] {
				continue
			}
			seen[p.Key] = true
			if _, err := fmt.Fprintf(w, "\n# from %s: %s, %d messages (%.0f%%)\n",
				b.Key, SafeLine(c.Label, 60), c.Count, c.Share*100); err != nil {
				return err
			}
			for _, ex := range c.Examples {
				if _, err := fmt.Fprintf(w, "#   e.g. %s\n", ex); err != nil {
					return err
				}
			}
			if p.Conflict != "" || p.Key == "" {
				if _, err := fmt.Fprintf(w, "#   not proposed: %s (%s)\n", p.Conflict, p.Key); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "# [[category]]\n# key         = %q\n# folder      = %q\n# description = \"\"\n# annual      = %t\n",
				p.Key, p.Folder, p.Annual); err != nil {
				return err
			}
		}
	}
	return nil
}
