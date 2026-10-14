package blob

import "testing"

func TestVerifyOffRequiresExistingRegularFile(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	s.SetVerifyMode(VerifyOff)
	sha, _ := writeVerifyBlob(t, s, KindRaw, tenant, bucket, []byte("present"))
	if ok, err := s.ContentMatches(KindRaw, tenant, bucket, sha, 999); err != nil || !ok {
		t.Fatalf("present: %v %v", ok, err)
	}
	missing := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if ok, err := s.ContentMatches(KindRaw, tenant, bucket, missing, 999); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
}
