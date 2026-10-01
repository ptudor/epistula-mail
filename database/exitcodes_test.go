package main

import "testing"

func TestExitCodeName(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{EX_OK, "EX_OK"},
		{EX_TEMPFAIL, "EX_TEMPFAIL"},
		{EX_NOUSER, "EX_NOUSER"},
		{EX_CANTCREAT, "EX_CANTCREAT"},
		{42, "UNKNOWN"},
		{-1, "UNKNOWN"},
	}
	for _, c := range cases {
		if got := ExitCodeName(c.code); got != c.want {
			t.Errorf("ExitCodeName(%d) = %q; want %q", c.code, got, c.want)
		}
	}
}

func TestPermanentVsTemporary(t *testing.T) {
	// Postfix's src/global/sys_exits.c maps ONLY EX_OSERR (71) and
	// EX_TEMPFAIL (75) to a 4.x.x defer; every other recognized sysexits code
	// bounces (5.x.x). This test is the ground-truth table.
	//
	// Bounce class: Postfix will not retry. Note EX_UNAVAILABLE, EX_SOFTWARE,
	// EX_OSFILE, EX_IOERR, EX_PROTOCOL, and EX_CONFIG are here — they were
	// wrongly classified as requeue before this fix (R-004).
	perm := []int{
		EX_USAGE, EX_DATAERR, EX_NOINPUT, EX_NOUSER, EX_NOHOST,
		EX_UNAVAILABLE, EX_SOFTWARE, EX_OSFILE, EX_CANTCREAT,
		EX_IOERR, EX_PROTOCOL, EX_NOPERM, EX_CONFIG,
	}
	for _, c := range perm {
		if !IsPermanentFailure(c) {
			t.Errorf("IsPermanentFailure(%s) = false; want true", ExitCodeName(c))
		}
		if IsTemporaryFailure(c) {
			t.Errorf("IsTemporaryFailure(%s) = true; want false (mutually exclusive)", ExitCodeName(c))
		}
	}

	// Requeue class: Postfix retries. ONLY these two.
	tmp := []int{EX_OSERR, EX_TEMPFAIL}
	for _, c := range tmp {
		if !IsTemporaryFailure(c) {
			t.Errorf("IsTemporaryFailure(%s) = false; want true", ExitCodeName(c))
		}
		if IsPermanentFailure(c) {
			t.Errorf("IsPermanentFailure(%s) = true; want false (mutually exclusive)", ExitCodeName(c))
		}
	}

	// EX_OK is neither permanent nor temporary failure.
	if IsPermanentFailure(EX_OK) || IsTemporaryFailure(EX_OK) {
		t.Error("EX_OK should be neither permanent nor temporary failure")
	}

	// Codes outside the sysexits range stay unclassified so the disposition
	// log surfaces them for a human.
	for _, c := range []int{999, -1, 200} {
		if IsPermanentFailure(c) || IsTemporaryFailure(c) {
			t.Errorf("out-of-range code %d should be unclassified", c)
		}
	}
}

func TestExitDispositionDescribesPostfixBehavior(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{EX_OK, "delivered"},
		{EX_NOUSER, "bounce"},
		{EX_CANTCREAT, "bounce"},
		{EX_TEMPFAIL, "requeue"},
		{EX_OSERR, "requeue"},   // the other defer-class code
		{EX_SOFTWARE, "bounce"}, // R-004: was wrongly "requeue" before
		{EX_CONFIG, "bounce"},   // R-004: was wrongly "requeue" before
		{999, "unclassified"},
	}
	for _, c := range cases {
		if got := exitDisposition(c.code); got != c.want {
			t.Errorf("exitDisposition(%d) = %q; want %q", c.code, got, c.want)
		}
	}
}
