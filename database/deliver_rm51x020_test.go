package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
)

func TestDeliveryRequiresExistingRoot(t *testing.T) {
	c, _, cleanup := deliverFixture(t)
	defer cleanup()
	c.Storage.Root = filepath.Join(t.TempDir(), "unmounted")
	if code := deliverBytes(t, c, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", "Subject: missing root\r\n\r\nbody\r\n"); code != EX_TEMPFAIL {
		t.Fatalf("delivery=%d", code)
	}
	if _, err := os.Stat(c.Storage.Root); !os.IsNotExist(err) {
		t.Fatalf("root created: %v", err)
	}
	if _, code := openBlobStoreFor(c, false); code != EX_TEMPFAIL {
		t.Fatalf("live import/gc opener=%d", code)
	}
	if _, err := os.Stat(c.Storage.Root); !os.IsNotExist(err) {
		t.Fatalf("root created: %v", err)
	}
}

func TestDeliveryChecksOnlyTargetTenantPermissions(t *testing.T) {
	c, _, cleanup := deliverFixture(t)
	defer cleanup()
	c.Production = true
	if err := os.Chmod(c.Storage.Root, 0750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if err := os.Mkdir(filepath.Join(c.Storage.Root, fmt.Sprintf("unrelated%d", i)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	s, code := openBlobStore(c, true)
	if code != EX_OK {
		t.Fatalf("delivery open=%d", code)
	}
	if err := s.CheckTenantPermissions(blob.Tenant("ra6x008")); err != nil {
		t.Fatal(err)
	}
	t.Logf("root + target check with 5000 unrelated tenants: %s", time.Since(start))
	if err := s.CheckPermissions(); err == nil {
		t.Fatal("full check failed to inspect unrelated permissions")
	}
	target := filepath.Join(c.Storage.Root, "ra6x008")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if code := deliverBytes(t, c, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", "Subject: target permissions\r\n\r\nbody\r\n"); code != EX_TEMPFAIL {
		t.Fatalf("unsafe target delivery=%d", code)
	}
	if err := os.Chmod(target, 0750); err != nil {
		t.Fatal(err)
	}
	if code := deliverBytes(t, c, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", "Subject: target permissions\r\n\r\nbody\r\n"); code != EX_OK {
		t.Fatalf("safe target delivery=%d", code)
	}
}
