package imapsess

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type authCatalogTracer struct{ catalogs, identities atomic.Int64 }

func (t *authCatalogTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == authProfileQuery {
		t.catalogs.Add(1)
	}
	if strings.Contains(d.SQL, "SELECT id,name,password_hash,disabled_at FROM mailboxes WHERE name=$1") {
		t.identities.Add(1)
	}
	return ctx
}
func (*authCatalogTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestTenLoginsShareCatalog(t *testing.T) {
	fixture := mutationFixture(t)
	cfg := fixture.be.Pool.Config()
	tracer := &authCatalogTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	be := &Backend{Pool: pool, Logger: fixture.be.Logger, StmtTimeout: time.Second, authVerify: func(string, string) (bool, error) { return false, nil }}
	for range 10 {
		s := be.NewSession()
		if err := s.Login("unknown", "wrong"); err == nil {
			t.Fatal("authenticated unknown")
		}
		s.Close()
	}
	if tracer.catalogs.Load() != 1 || tracer.identities.Load() != 10 {
		t.Fatalf("catalog=%d identity=%d", tracer.catalogs.Load(), tracer.identities.Load())
	}
	// Credential state remains fresh even while the workset is cached.
	if _, err := pool.Exec(context.Background(), `UPDATE mailboxes SET disabled_at=now() WHERE id=$1`, fixture.mailboxID); err != nil {
		t.Fatal(err)
	}
	identity, _, err := be.loadLoginIdentity(context.Background(), fixture.mailboxName)
	if err != nil || identity.disabled == nil {
		t.Fatalf("stale identity %+v: %v", identity, err)
	}
	be.authProfilesMu.Lock()
	be.authProfilesUntil = time.Time{}
	be.authProfilesMu.Unlock()
	if _, _, err := be.loadLoginIdentity(context.Background(), "unknown"); err != nil {
		t.Fatal(err)
	}
	if tracer.catalogs.Load() != 2 {
		t.Fatal("catalog did not refresh after expiry")
	}
}
