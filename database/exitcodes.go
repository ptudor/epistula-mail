package main

// sysexits.h constants. Postfix interprets these to decide bounce vs. requeue
// behavior for a pipe transport, so the values must match BSD sysexits exactly.
const (
	EX_OK          = 0
	EX_USAGE       = 64
	EX_DATAERR     = 65
	EX_NOINPUT     = 66
	EX_NOUSER      = 67
	EX_NOHOST      = 68
	EX_UNAVAILABLE = 69
	EX_SOFTWARE    = 70
	EX_OSERR       = 71
	EX_OSFILE      = 72
	EX_CANTCREAT   = 73
	EX_IOERR       = 74
	EX_TEMPFAIL    = 75
	EX_PROTOCOL    = 76
	EX_NOPERM      = 77
	EX_CONFIG      = 78
)

// ExitCodeName returns the symbolic name for a sysexits code, or "UNKNOWN".
// Used for human-readable logging so the operator does not have to memorize
// the numeric table when reading the mail log.
func ExitCodeName(code int) string {
	switch code {
	case EX_OK:
		return "EX_OK"
	case EX_USAGE:
		return "EX_USAGE"
	case EX_DATAERR:
		return "EX_DATAERR"
	case EX_NOINPUT:
		return "EX_NOINPUT"
	case EX_NOUSER:
		return "EX_NOUSER"
	case EX_NOHOST:
		return "EX_NOHOST"
	case EX_UNAVAILABLE:
		return "EX_UNAVAILABLE"
	case EX_SOFTWARE:
		return "EX_SOFTWARE"
	case EX_OSERR:
		return "EX_OSERR"
	case EX_OSFILE:
		return "EX_OSFILE"
	case EX_CANTCREAT:
		return "EX_CANTCREAT"
	case EX_IOERR:
		return "EX_IOERR"
	case EX_TEMPFAIL:
		return "EX_TEMPFAIL"
	case EX_PROTOCOL:
		return "EX_PROTOCOL"
	case EX_NOPERM:
		return "EX_NOPERM"
	case EX_CONFIG:
		return "EX_CONFIG"
	default:
		return "UNKNOWN"
	}
}

// IsPermanentFailure reports whether Postfix will bounce (no retry) when the
// LDA exits with this code.
//
// Postfix's src/global/sys_exits.c maps ONLY EX_OSERR (71) and EX_TEMPFAIL
// (75) to a 4.x.x (defer/requeue) status; EVERY other recognized sysexits
// code maps to 5.x.x and BOUNCES. So the permanent set is every known
// sysexits code except EX_OK, EX_OSERR, and EX_TEMPFAIL. Codes outside the
// sysexits range are left unclassified (neither) so the disposition log flags
// an unexpected value for a human rather than guessing.
//
// This is why the deliver path must never return EX_SOFTWARE/EX_CONFIG/etc.
// for a transient condition: to Postfix those are all bounces, and the mail
// is lost. When in doubt at a routine deliver error site, return EX_TEMPFAIL.
func IsPermanentFailure(code int) bool {
	switch code {
	case EX_USAGE, EX_DATAERR, EX_NOINPUT, EX_NOUSER, EX_NOHOST,
		EX_UNAVAILABLE, EX_SOFTWARE, EX_OSFILE, EX_CANTCREAT,
		EX_IOERR, EX_PROTOCOL, EX_NOPERM, EX_CONFIG:
		return true
	default:
		return false
	}
}

// IsTemporaryFailure reports whether Postfix will requeue and retry. Per
// Postfix's sys_exits.c only EX_OSERR and EX_TEMPFAIL defer; everything else
// non-zero bounces (see IsPermanentFailure).
func IsTemporaryFailure(code int) bool {
	switch code {
	case EX_OSERR, EX_TEMPFAIL:
		return true
	default:
		return false
	}
}
