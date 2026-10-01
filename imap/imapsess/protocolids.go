package imapsess

import (
	"github.com/emersion/go-imap/v2"
	"github.com/ptudor/epistula-mail/database/storage"
)

func protocolIDs(values ...int64) error {
	for _, value := range values {
		if storage.CheckProtocolID("IMAP identifier", value) != nil {
			return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "folder identifiers exhausted or invalid; operator recovery required"}
		}
	}
	return nil
}
