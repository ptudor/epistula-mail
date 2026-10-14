package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestTextParametersRejectNULWithoutErrorLogs(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.passQueueAvailable = true
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	paths := []string{
		"/v1/mailboxes/ali%00ce/folders",
		"/v1/mailboxes/alice/folders/IN%00BOX/messages",
		fmt.Sprintf("/v1/messages/%d/attachments/1%%002", f.aliceMsgIDs[0]),
	}
	for _, param := range []string{"mailbox", "folder", "q", "tag", "category", "annotated_by", "not_annotated_by", "flag", "not_flag"} {
		paths = append(paths, "/v1/search?"+param+"=x%00y")
	}
	for _, param := range []string{"attachment_after", "annotation_after", "annotation_model"} {
		paths = append(paths, fmt.Sprintf("/v1/messages/%d?metadata=inspection&%s=x%%00y", f.aliceMsgIDs[0], param))
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			wantProblem(t, f.do(http.MethodGet, path, f.classifierToken, nil), http.StatusUnprocessableEntity)
		})
	}
	wantProblem(t, f.do(http.MethodPost, "/v1/pass-required/prune", f.classifierToken, strings.NewReader(`{"model":"m","mailbox":"a\u0000b"}`)), http.StatusUnprocessableEntity)
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("NUL requests logged errors: %s", logs.String())
	}
}
