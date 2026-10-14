package main

import (
	"errors"
	"github.com/jackc/puddle/v2"
	"github.com/ptudor/epistula-mail/database/storage"
	"os"
)

func maintenanceWalkExit(err error) int {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return EX_OSERR
	}
	if storage.IsRetryable(err) || errors.Is(err, storage.ErrMailboxInMaintenance) || errors.Is(err, puddle.ErrClosedPool) {
		return EX_TEMPFAIL
	}
	return EX_OSERR
}
