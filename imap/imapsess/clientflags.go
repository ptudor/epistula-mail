package imapsess

import (
	"errors"

	"github.com/emersion/go-imap/v2"
	"github.com/ptudor/epistula-mail/database/imapflags"
)

func validateClientFlags(flags []string, capKeywords bool) *imap.Error {
	for _, flag := range flags {
		if err := imapflags.ValidateClientFlag(flag); err != nil {
			if errors.Is(err, imapflags.ErrClientFlagLimit) {
				return clientFlagLimit()
			}
			return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "invalid client flag"}
		}
	}
	if capKeywords && !imapflags.ClientKeywordsWithinLimit(flags) {
		return clientFlagLimit()
	}
	return nil
}

func clientFlagLimit() *imap.Error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "message keyword limit exceeded"}
}

// normalizedFlagsSQL is shared by STORE and implicit Seen writes. Arguments
// are fixed SQL fragments, never client input.
func normalizedFlagsSQL(array, predicate string) string {
	if predicate != "" {
		predicate = " WHERE " + predicate
	}
	return `(SELECT COALESCE(array_agg(DISTINCT f ORDER BY f), '{}'::text[]) FROM unnest(` + array + `) f` + predicate + `)`
}
