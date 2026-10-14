package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureFolderRejectsInvalidNamesBeforeSQL(t *testing.T) {
	for _, name := range []string{"a//b", "a/", strings.Repeat("x", 300), "\t"} {
		if _, _, err := EnsureFolder(context.Background(), nil, 1, name, nil); !errors.Is(err, ErrInvalidFolderName) {
			t.Errorf("%q: %v", name, err)
		}
	}
}
