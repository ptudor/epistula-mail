// Package recipients resolves envelope addresses against the domains,
// aliases, and domain_acl tables.
//
// Resolution order (see database CLAUDE.md "Delivery flow"):
//
//  1. Denylist. If (domain_id, localpart, 'deny') exists in domain_acl,
//     reject — deny wins over everything, even an explicit alias.
//  2. Exact alias. aliases(domain_id, localpart) hit → deliver to that mailbox.
//  3. Wildcard catchall. If domains.is_wildcard is true OR a catchall alias
//     (localpart=”) exists, route to the catchall mailbox. When the domain
//     has any (domain_id, _, 'allow') rows configured, the localpart must
//     match an allow row first — otherwise reject as not-allowlisted.
//  4. No match → ErrNoMatch.
package recipients

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/unicode/norm"
)

// ErrUnknownDomain is returned when the domain part has no row in `domains`.
var ErrUnknownDomain = errors.New("recipients: unknown domain")

// ErrNoMatch is returned when neither an exact alias nor a wildcard catchall
// resolves to a mailbox.
var ErrNoMatch = errors.New("recipients: no alias or catchall for address")

// ErrInvalidAddress is returned for malformed envelope addresses.
var ErrInvalidAddress = errors.New("recipients: invalid envelope address")

// ErrDenied is returned when the address matches a deny row in domain_acl.
// Deny takes precedence over every other route, including explicit aliases.
var ErrDenied = errors.New("recipients: address denied by domain ACL")

// ErrNotAllowlisted is returned when the domain has at least one allow row
// configured but the requested localpart does not match any of them and
// resolution would otherwise have fallen through to the wildcard catchall.
var ErrNotAllowlisted = errors.New("recipients: address not on domain allowlist")

// Match is the resolved (mailbox, alias) for an envelope address.
type Match struct {
	MailboxID int64
	// MailboxName is the canonical mailboxes.name — the on-disk blob tenant.
	// Threaded through delivery so the blob writer, the ingest advisory-lock
	// key, and the GC walker all agree on the same string.
	MailboxName string
	AliasID     int64  // 0 when matched via the is_wildcard catchall (no aliases row)
	Localpart   string // normalized localpart from the envelope address
	Domain      string // normalized domain from the envelope address
	IsCatchall  bool   // true when matched via localpart='' alias or is_wildcard
}

// Resolver looks aliases up in the pool.
type Resolver struct {
	pool *pgxpool.Pool
}

// New returns a Resolver that uses the given pool.
func New(pool *pgxpool.Pool) *Resolver {
	return &Resolver{pool: pool}
}

// SplitAddress splits "local@domain" into normalized lowercase NFC components.
// Returns ErrInvalidAddress for empty input, missing '@', empty localpart, or
// empty domain. Trailing '.' is stripped from the domain (FQDN-equivalent).
func SplitAddress(addr string) (localpart, domain string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", ErrInvalidAddress
	}
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", "", ErrInvalidAddress
	}
	rawLocal := addr[:at]
	rawDomain := strings.TrimRight(addr[at+1:], ".")
	if rawDomain == "" {
		return "", "", ErrInvalidAddress
	}
	localpart = norm.NFC.String(strings.ToLower(rawLocal))
	domain = norm.NFC.String(strings.ToLower(rawDomain))
	return localpart, domain, nil
}

// Resolve looks up the mailbox for an envelope address.
func (r *Resolver) Resolve(ctx context.Context, envelopeTo string) (Match, error) {
	local, domain, err := SplitAddress(envelopeTo)
	if err != nil {
		return Match{}, err
	}
	base := Match{Localpart: local, Domain: domain}

	var (
		domainID   int64
		isWildcard bool
	)
	if err := r.pool.QueryRow(ctx,
		`SELECT id, is_wildcard FROM domains WHERE name = $1`,
		domain,
	).Scan(&domainID, &isWildcard); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return base, ErrUnknownDomain
		}
		return Match{}, fmt.Errorf("lookup domain: %w", err)
	}

	// Step 1: deny wins absolutely.
	var denied bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM domain_acl
			 WHERE domain_id = $1 AND localpart = $2 AND kind = 'deny'
		)`,
		domainID, local,
	).Scan(&denied); err != nil {
		return Match{}, fmt.Errorf("lookup deny: %w", err)
	}
	if denied {
		return base, ErrDenied
	}

	// Step 2: exact alias.
	var (
		aliasID     int64
		mailboxID   int64
		mailboxName string
		matchedLP   string
	)
	err = r.pool.QueryRow(ctx,
		`SELECT a.id, a.mailbox_id, m.name, a.localpart
		   FROM aliases a
		   JOIN mailboxes m ON m.id = a.mailbox_id
		  WHERE a.domain_id = $1 AND a.localpart = $2`,
		domainID, local,
	).Scan(&aliasID, &mailboxID, &mailboxName, &matchedLP)
	if err == nil {
		return Match{
			MailboxID:   mailboxID,
			MailboxName: mailboxName,
			AliasID:     aliasID,
			Localpart:   local,
			Domain:      domain,
			IsCatchall:  false,
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Match{}, fmt.Errorf("lookup exact alias: %w", err)
	}

	// Step 3: catchall + allowlist gate. A catchall is a localpart='' alias,
	// present for both is_wildcard domains and standard domains that opted into
	// a catchall. Look it up first, then apply the allowlist gate whenever the
	// domain would accept beyond its exact aliases — is_wildcard OR a catchall
	// alias exists. The gate was previously wrapped in `if isWildcard`, so a
	// non-wildcard domain with a catchall alias bypassed the allowlist
	// entirely (R-026). A standard domain with neither wildcard nor catchall
	// never reaches the gate, so its allow rows stay inert as documented.
	err = r.pool.QueryRow(ctx,
		`SELECT a.id, a.mailbox_id, m.name
		   FROM aliases a
		   JOIN mailboxes m ON m.id = a.mailbox_id
		  WHERE a.domain_id = $1 AND a.localpart = ''`,
		domainID,
	).Scan(&aliasID, &mailboxID, &mailboxName)
	hasCatchall := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Match{}, fmt.Errorf("lookup catchall: %w", err)
	}

	if isWildcard || hasCatchall {
		var allowCount int
		if err := r.pool.QueryRow(ctx,
			`SELECT count(*) FROM domain_acl
			  WHERE domain_id = $1 AND kind = 'allow'`,
			domainID,
		).Scan(&allowCount); err != nil {
			return Match{}, fmt.Errorf("count allow rows: %w", err)
		}
		if allowCount > 0 {
			var allowed bool
			if err := r.pool.QueryRow(ctx,
				`SELECT EXISTS(
					SELECT 1 FROM domain_acl
					 WHERE domain_id = $1 AND localpart = $2 AND kind = 'allow'
				)`,
				domainID, local,
			).Scan(&allowed); err != nil {
				return Match{}, fmt.Errorf("lookup allow: %w", err)
			}
			if !allowed {
				return base, ErrNotAllowlisted
			}
		}
	}

	if hasCatchall {
		return Match{
			MailboxID:   mailboxID,
			MailboxName: mailboxName,
			AliasID:     aliasID,
			Localpart:   local,
			Domain:      domain,
			IsCatchall:  true,
		}, nil
	}

	return base, ErrNoMatch
}
