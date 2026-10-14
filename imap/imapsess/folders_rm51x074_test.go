package imapsess

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestFolderValidatorsAgree(t *testing.T) {
	for _, name := range []string{"", "Projects", "/a", "a//b", "a/", "a\tb", "inbox/Foo", strings.Repeat("a", 256)} {
		if (validateFolderName(name) == nil) != (storage.ValidateFolderName(name) == nil) {
			t.Errorf("validators disagree on %q", name)
		}
	}
}

func TestFolderWriteDeclarationAndInboxPrefix(t *testing.T) {
	s, _ := appendFixture(t)
	defer s.Close()
	if err := s.Create("Projects/", nil); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !reflect.DeepEqual(got, []string{"Projects"}) {
		t.Fatal(got)
	}
	if err := s.Delete("Projects"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/a", "a//b"} {
		var ie *imap.Error
		if err := s.Create(name, nil); !errors.As(err, &ie) || ie.Code != imap.ResponseCodeCannot {
			t.Fatalf("CREATE %q: %v", name, err)
		}
	}
	if err := s.Create("inbox/Foo", nil); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !reflect.DeepEqual(got, []string{"INBOX", "INBOX/Foo"}) {
		t.Fatal(got)
	}
	if _, err := s.Append("inbox/Foo", newLiteral([]byte(appendRawMsg)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select("inbox/Foo", nil); err != nil {
		t.Fatal(err)
	}
	var ie *imap.Error
	if _, err := s.Copy(imap.SeqSetNum(1), "/x"); !errors.As(err, &ie) || ie.Code != imap.ResponseCodeCannot {
		t.Fatalf("COPY: %v", err)
	}
	if _, err := s.Select("inbox/foo", nil); !errors.As(err, &ie) || ie.Code != imap.ResponseCodeNonExistent {
		t.Fatalf("SELECT case-sensitive child: %v", err)
	}
}
