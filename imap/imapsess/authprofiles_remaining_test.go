package imapsess

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestAuthenticationCatalogBound(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	for i := 1; i <= maxAuthProfiles; i++ {
		hash := fmt.Sprintf("$argon2id$v=19$m=8,t=%d,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", i)
		if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES($1,$2)`, fmt.Sprintf("class%d", i), hash); err != nil {
			t.Fatal(err)
		}
	}
	if profiles, err := readAuthProfiles(ctx, db.Pool()); err != nil || len(profiles) != maxAuthProfiles {
		t.Fatal("exact catalog bound", len(profiles), err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('extra','$argon2id$v=19$m=8,t=99,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')`); err != nil {
		t.Fatal(err)
	}
	if _, err := readAuthProfiles(ctx, db.Pool()); err == nil {
		t.Fatal("excessive cost catalog accepted")
	}
}

func TestAuthenticationRunsIdenticalCostWorksets(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	be := &Backend{Pool: db.Pool(), BlobStore: blob.NewStore(t.TempDir()), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), StmtTimeout: 10 * time.Second, AuthBudget: NewAuthBudget(64)}
	hashes := map[string]string{}
	for i, p := range []auth.Params{
		{Memory: 8, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 32, Iterations: 4, Parallel: 2, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Iterations: 1, Parallel: 1, SaltLen: 8, KeyLen: 64},
	} {
		hash, err := auth.HashPassword("right", p)
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("cost%d", i)
		hashes[name] = hash
		if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES($1,$2)`, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash,disabled_at) VALUES('disabled',$1,now()),('malformed','broken',NULL)`, hashes["cost2"]); err != nil {
		t.Fatal(err)
	}
	profiles, err := readAuthProfiles(ctx, db.Pool())
	if err != nil || len(profiles) != 3 {
		t.Fatal("cost catalog", len(profiles), err)
	}
	var expected []auth.EncodedParams
	for _, p := range profiles {
		expected = append(expected, p.params)
	}
	for _, name := range []string{"unknown", "disabled", "malformed", "cost0", "cost1", "cost2"} {
		var calls []auth.EncodedParams
		var work uint64
		be.authVerify = func(password, encoded string) (bool, error) {
			p, err := auth.ParseEncodedParams(encoded)
			if err != nil {
				return false, err
			}
			calls = append(calls, p)
			work += uint64(p.Memory) * uint64(p.Iterations)
			if be.AuthBudget.sem.TryAcquire(1) {
				be.AuthBudget.sem.Release(1)
				t.Error("verification escaped the common maximum reservation")
			}
			// Even a matching dummy must never authenticate. Actual hashes still
			// verify against the real password; this also tests successful logins.
			for _, hash := range hashes {
				if encoded == hash {
					return auth.VerifyPassword(password, encoded)
				}
			}
			return true, nil
		}
		s := be.NewSession()
		err := s.Login(name, "wrong")
		s.Close()
		if err == nil {
			t.Fatal("failed/dummy credential authenticated", name)
		}
		if !reflect.DeepEqual(calls, expected) || work != 8+32*4+64 {
			t.Fatalf("%s got different KDF work: %v / %d", name, calls, work)
		}
	}
	be.authVerify = nil
	for name := range hashes {
		s := be.NewSession()
		if err := s.Login(name, "right"); err != nil {
			t.Fatal("historical password rejected", name, err)
		}
		s.Close()
	}
	// A supported outlier above the configured admission budget refuses all
	// categories equally before starting any KDF, including unknown accounts.
	be.AuthBudget = NewAuthBudget(32)
	calls := 0
	be.authVerify = func(string, string) (bool, error) { calls++; return false, nil }
	for _, name := range []string{"unknown", "disabled", "malformed", "cost0", "cost2"} {
		s := be.NewSession()
		if err := s.Login(name, "wrong"); err == nil {
			t.Fatal(name)
		}
		s.Close()
	}
	if calls != 0 {
		t.Fatal("over-budget work started")
	}
	if err := AuditAuthProfiles(ctx, db.Pool(), be.AuthBudget, be.Logger); err == nil {
		t.Fatal("startup admitted over-budget catalog")
	}
}

func TestAuthenticationProfileCancellationAndEncodings(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	p := auth.Params{Memory: 8, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32}
	hash, err := auth.HashPassword("right", p)
	if err != nil {
		t.Fatal(err)
	}
	// Go's valid historical base64 spelling permits CR/LF in salt and key.
	parts := strings.Split(hash, "$")
	parts[4] = parts[4][:4] + "\r\n" + parts[4][4:]
	parts[5] = parts[5][:5] + "\n" + parts[5][5:]
	hash = strings.Join(parts, "$")
	if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('old',$1),('bad','$argon2id$v=19$m=1048576,t=100,p=64$INVALID!$INVALID!')`, hash); err != nil {
		t.Fatal(err)
	}
	be := &Backend{Pool: db.Pool(), BlobStore: blob.NewStore(t.TempDir()), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), StmtTimeout: 10 * time.Second, AuthBudget: NewAuthBudget(8)}
	profiles, err := readAuthProfiles(ctx, db.Pool())
	if err != nil || len(profiles) != 1 || profiles[0].params.Memory != 8 {
		t.Fatal("valid CRLF/invalid encoding catalog", profiles, err)
	}
	s := be.NewSession()
	if err := s.Login("old", "right"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := be.AuthBudget.sem.Acquire(ctx, 8); err != nil {
		t.Fatal(err)
	}
	defer be.AuthBudget.sem.Release(8)
	for _, name := range []string{"unknown", "old", "bad"} {
		s := be.NewSession()
		done := make(chan error, 1)
		go func() { done <- s.Login(name, "wrong") }()
		deadline := time.Now().Add(5 * time.Second)
		for be.AuthBudget.waiting.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if be.AuthBudget.waiting.Load() == 0 {
			t.Fatal("login did not queue")
		}
		s.sessCancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled login authenticated")
			}
		case <-time.After(time.Second):
			t.Fatal("canceled login stayed queued")
		}
		s.Close()
	}
}

// This TCP test uses a disposable database.
// Timing distributions supplement the deterministic workset proof;
// scheduler noise is not judged with a flaky single-sample ratio.
func TestAuthenticationMixedCostTCPDistributions(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	names := []string{"unknown", "disabled", "malformed", "low", "default", "high"}
	for i, p := range []auth.Params{
		{Memory: 8 * 1024, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32},
		auth.DefaultParams(),
		{Memory: 128 * 1024, Iterations: 2, Parallel: 2, SaltLen: 16, KeyLen: 32},
	} {
		hash, err := auth.HashPassword("right", p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES($1,$2)`, names[i+3], hash); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if _, err := db.Pool().Exec(ctx, `INSERT INTO mailboxes(name,password_hash,disabled_at) VALUES('disabled',$1,now()),('malformed','invalid',NULL)`, hash); err != nil {
				t.Fatal(err)
			}
		}
	}
	be := &Backend{Pool: db.Pool(), BlobStore: blob.NewStore(t.TempDir()), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), StmtTimeout: 10 * time.Second, AuthBudget: NewAuthBudget(128 * 1024)}
	if err := AuditAuthProfiles(ctx, db.Pool(), be.AuthBudget, slog.Default()); err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(&imapserver.Options{NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return be.NewSession(), nil, nil
	}, InsecureAuth: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve(ln)
	sample := func(name string) time.Duration {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		fmt.Fprintf(conn, "a LOGIN %s wrong\r\n", name)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(name, err)
			}
			if strings.HasPrefix(line, "a ") {
				if !strings.Contains(line, "NO [AUTHENTICATIONFAILED]") {
					t.Fatal("non-generic failure", line)
				}
				return time.Since(start)
			}
		}
	}
	for _, name := range names {
		sample(name)
	} // warm every path
	count := 4
	if raw := os.Getenv("MAIL_VERIFY_AUTH_SAMPLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 4 || n > 100 {
			t.Fatal("invalid sample count")
		}
		count = n
	}
	durations := map[string][]time.Duration{}
	rng := rand.New(rand.NewSource(6063))
	for i := 0; i < count; i++ {
		for _, j := range rng.Perm(len(names)) {
			name := names[j]
			durations[name] = append(durations[name], sample(name))
		}
	}
	for _, name := range names {
		values := durations[name]
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		t.Logf("auth timing category=%s n=%d min=%s median=%s p95=%s", name, len(values), values[0], values[len(values)/2], values[(len(values)*95+99)/100-1])
	}
}
