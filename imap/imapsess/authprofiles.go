package imapsess

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/epistula-mail/database/auth"
)

// Every admitted LOGIN runs the same ordered set of supported cost classes.
// One class uses the real credential when applicable; all others use dummies.
// A floor alone cannot equalize work slower than that floor. This policy keeps
// existing PHC verification intact while charging identical aggregate KDF work
// to unknown, disabled, malformed, wrong-password and successful accounts.
const maxAuthProfiles = 32
const maxAuthProfileEncodings = 256

type authProfile struct {
	params auth.EncodedParams
	dummy  string
	cost   int64
}
type loginIdentity struct {
	id         int64
	name, hash string
	disabled   *time.Time
}
type authProfileQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Only cost fields and encoded lengths cross the wire, never other users'
// salts/digests. Strip CR/LF exactly as Go's base64 decoder does. Unsupported
// encodings cannot request KDF work. Bounded distinct encodings/profile counts
// turn an excessive catalog into a uniform service-admission failure.
const authProfileQuery = `WITH parts AS (
 SELECT string_to_array(password_hash,'$') AS p FROM mailboxes WHERE octet_length(password_hash)<=4096
), costs AS (
 SELECT p[3] AS version,p[4] AS parameters,
 replace(replace(p[5],chr(13),''),chr(10),'') AS salt,
 replace(replace(p[6],chr(13),''),chr(10),'') AS digest
 FROM parts WHERE cardinality(p)=6 AND p[1]='' AND p[2]='argon2id'
)
SELECT DISTINCT version,parameters,length(salt),length(digest) FROM costs
WHERE salt ~ '^[A-Za-z0-9+/]+$' AND digest ~ '^[A-Za-z0-9+/]+$'
 AND length(salt)%4<>1 AND length(digest)%4<>1 LIMIT 257`

func readAuthProfiles(ctx context.Context, q authProfileQuerier) ([]authProfile, error) {
	rows, err := q.Query(ctx, authProfileQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	unique := map[auth.EncodedParams]authProfile{}
	encodings := 0
	for rows.Next() {
		var version, parameters string
		var saltLen, keyLen int
		if err := rows.Scan(&version, &parameters, &saltLen, &keyLen); err != nil {
			return nil, err
		}
		encodings++
		if encodings > maxAuthProfileEncodings {
			return nil, fmt.Errorf("authentication has too many PHC cost encodings; normalize stored hash formats")
		}
		sample := "$argon2id$" + version + "$" + parameters + "$" + strings.Repeat("A", saltLen) + "$" + strings.Repeat("A", keyLen)
		p, err := auth.ParseEncodedParams(sample)
		if err != nil {
			continue
		}
		// Canonicalize valid historical spelling variations to the same class.
		dummy := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.Memory, p.Iterations, p.Parallel,
			base64.RawStdEncoding.EncodeToString(make([]byte, p.SaltLen)), base64.RawStdEncoding.EncodeToString(make([]byte, p.KeyLen)))
		cost, err := verifyCost(dummy)
		if err != nil {
			return nil, err
		}
		unique[p] = authProfile{params: p, dummy: dummy, cost: cost}
		if len(unique) > maxAuthProfiles {
			return nil, fmt.Errorf("authentication has more than %d cost classes; normalize historical password costs", maxAuthProfiles)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(unique) == 0 {
		p, _ := auth.ParseEncodedParams(dummyPasswordHash)
		cost, _ := verifyCost(dummyPasswordHash)
		unique[p] = authProfile{params: p, dummy: dummyPasswordHash, cost: cost}
	}
	profiles := make([]authProfile, 0, len(unique))
	for _, p := range unique {
		profiles = append(profiles, p)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].dummy < profiles[j].dummy })
	return profiles, nil
}

func loadLoginIdentity(ctx context.Context, pool *pgxpool.Pool, username string) (loginIdentity, []authProfile, error) {
	var identity loginIdentity
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return identity, nil, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT id,name,password_hash,disabled_at FROM mailboxes WHERE name=$1`, username).Scan(&identity.id, &identity.name, &identity.hash, &identity.disabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return identity, nil, err
	}
	profiles, err := readAuthProfiles(ctx, tx)
	if err != nil {
		return identity, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return identity, nil, err
	}
	return identity, profiles, nil
}

func profileReservation(profiles []authProfile) int64 {
	var cost int64
	for _, p := range profiles {
		cost = max(cost, p.cost)
	}
	return cost
}

func (s *Session) verifyProfiles(password string, identity loginIdentity, profiles []authProfile) (bool, error) {
	ctx := s.sessCtx
	if ctx == nil {
		ctx = context.Background()
	}
	params, parseErr := auth.ParseEncodedParams(identity.hash)
	verify := s.be.authVerify
	if verify == nil {
		verify = auth.VerifyPassword
	}
	// Reserve the largest scratch allocation for the entire sequential set.
	// Every category therefore has identical admission/queue behavior too.
	return s.be.AuthBudget.verifyFunc(ctx, profileReservation(profiles), func() (bool, error) {
		matched := false
		for _, p := range profiles {
			if ctx.Err() != nil {
				return false, errAuthBudgetExhausted
			}
			encoded := p.dummy
			real := parseErr == nil && identity.id != 0 && params == p.params
			if real {
				encoded = identity.hash
			}
			ok, err := verify(password, encoded)
			if err != nil {
				return false, err
			}
			if real && ok {
				matched = true
			}
		}
		if ctx.Err() != nil {
			return false, errAuthBudgetExhausted
		}
		return matched && identity.disabled == nil, nil
	})
}

// AuditAuthProfiles logs the supported cost catalog at startup without
// credentials. A cost the configured global budget cannot admit refuses the
// whole service uniformly; it must never make only one username's path cheap.
func AuditAuthProfiles(ctx context.Context, pool *pgxpool.Pool, budget *AuthBudget, logger *slog.Logger) error {
	profiles, err := readAuthProfiles(ctx, pool)
	if err != nil {
		return err
	}
	reservation := profileReservation(profiles)
	if budget != nil && reservation > budget.LimitKiB() {
		return fmt.Errorf("authentication cost requires %d KiB but budget is %d; raise the budget or reset outlier passwords to the configured common costs", reservation, budget.LimitKiB())
	}
	costs := make([]string, 0, len(profiles))
	for _, p := range profiles {
		costs = append(costs, fmt.Sprintf("m=%d,t=%d,p=%d,salt=%d,key=%d", p.params.Memory, p.params.Iterations, p.params.Parallel, p.params.SaltLen, p.params.KeyLen))
	}
	logger.Info("authentication common-work policy", "cost_classes", costs, "reservation_kib", reservation)
	return nil
}
