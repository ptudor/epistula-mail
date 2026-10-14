package blob

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureContentAtomicForReaders(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	good := bytes.Repeat([]byte("correct body\n"), 100000)
	sha, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, good)
	if err := os.WriteFile(path, []byte("broken"), 0640); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	var reads atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			f, err := os.Open(path)
			if err != nil {
				done <- err
				return
			}
			f.Close()
			reads.Add(1)
		}
	}()
	for reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	repaired, err := s.EnsureContent(KindRaw, tenant, bucket, sha, good)
	close(stop)
	if readErr := <-done; readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil || !repaired {
		t.Fatalf("repair %v %v", repaired, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, good) {
		t.Fatal("bad final content", err)
	}
	t.Logf("%d concurrent opens, no missing file", reads.Load())
}

func TestSymlinkTenantPermissionsAndTmpSweep(t *testing.T) {
	s, _, _ := verifyStore(t)
	target := t.TempDir()
	if err := os.Chmod(s.root, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.root, "alice")); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(target, "tmp")
	if err := os.Mkdir(tmp, 0750); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(tmp, "blob-old")
	if err := os.WriteFile(old, []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)
	if n, err := s.SweepStaleTmp(24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
	if err := s.CheckPermissions(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckPermissions(); err == nil {
		t.Fatal("symlinked tenant permission violation ignored")
	}
	var unknown []string
	s.SetOnUnknownSubtree(func(n string) { unknown = append(unknown, n) })
	if err := os.Symlink(filepath.Join(target, "absent"), filepath.Join(s.root, "bob")); err != nil {
		t.Fatal(err)
	}
	if err := s.Walk(KindRaw, func(Kind, Tenant, Bucket, string, string, os.FileInfo) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "bob" {
		t.Fatal(unknown)
	}
}
