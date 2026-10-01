package main

// A page's memory cost is its BYTES, not its rows (RA6X-040).
//
// Export buffered a full page of complete text bodies plus every annotation
// row before writing its first byte. With the default page of 500 and accepted
// messages up to 50 MiB, one legitimate archive request could ask for tens of
// gigabytes of resident memory — and the two-stream semaphore caps requests,
// not their size, so two of them is all it takes. There is no need for a
// giant single row: many permitted rows suffice. Text-enabled list pages had
// the same row-count-only bound.
//
// The bound below is on aggregate retained bytes, applied while scanning. It
// preserves completeness: a page cut short by the budget resumes from its own
// cursor, so every row still arrives exactly once, in the same order, and no
// message body is truncated.

// defaultMaxPageBytes is the retained-bytes budget when none is configured.
// 32 MiB comfortably holds hundreds of ordinary messages while bounding the
// pathological case to something a server can hold several of at once.
const defaultMaxPageBytes = 32 << 20

// maxPageBytes resolves the configured budget.
func (s *server) maxPageBytes() int64 {
	if n := s.cfg.Limits.MaxPageBytes; n > 0 {
		return int64(n)
	}
	return defaultMaxPageBytes
}

// pageBudget tracks how many bytes of a page have been retained so far.
//
// The forward-progress rule is the important part: the FIRST row of a page is
// always admitted, however large. Without it a message bigger than the whole
// budget could never be returned, and an export would stall on it forever
// rather than merely using a lot of memory once.
type pageBudget struct {
	limit int64
	used  int64
	rows  int
}

func newPageBudget(limit int64) *pageBudget {
	if limit <= 0 {
		limit = defaultMaxPageBytes
	}
	return &pageBudget{limit: limit}
}

// admit reports whether a row of n bytes may join this page. A false result
// means the page is full: the caller must stop scanning and resume from the
// cursor, not drop the row.
func (b *pageBudget) admit(n int64) bool {
	if b.rows > 0 && b.used+n > b.limit {
		return false
	}
	b.rows++
	b.used += n
	return true
}

// itemBytes estimates the memory one scanned message retains. Only the
// variable-length fields matter; the fixed scalars are noise beside a body.
func itemBytes(m *messageItem) int64 {
	n := strLen(m.Subject) + strLen(m.From) + strLen(m.MessageID) + strLen(m.InReplyTo) +
		int64(len(m.Mailbox)+len(m.Folder))
	for _, a := range m.To {
		n += int64(len(a))
	}
	for _, a := range m.Cc {
		n += int64(len(a))
	}
	for _, f := range m.Flags {
		n += int64(len(f))
	}
	n += strLen(m.TextBody)
	for i := range m.Attachments {
		a := &m.Attachments[i]
		n += int64(len(a.PartNumber)+len(a.ContentType)+len(a.SHA256)) +
			strLen(a.Filename) + strLen(a.ContentID) + strLen(a.Disposition)
	}
	return n
}

// strLen is len for an optional string.
func strLen(p *string) int64 {
	if p == nil {
		return 0
	}
	return int64(len(*p))
}

// annotationBytes estimates what one annotation row retains.
func annotationBytes(a *annotationItem) int64 {
	n := int64(len(a.Model))
	for _, t := range a.Tags {
		n += int64(len(t))
	}
	return n + strLen(a.Category) + strLen(a.Summary)
}
