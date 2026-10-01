package main

import (
	"context"
	"crypto/tls"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"

	"github.com/ptudor/epistula-mail/imap/imapsess"
)

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx, cfg)
	if err != nil {
		slog.Error("postgres", "err", err)
		return EX_OSERR
	}
	defer pool.Close()

	// Dedicated pool for IDLE's LISTEN connections (idle_conn_pool_size)
	// so many simultaneous IDLE sessions can't starve query connections.
	var idlePool *pgxpool.Pool
	if cfg.Postgres.IdleConns > 0 {
		idlePool, err = openIdlePool(ctx, cfg)
		if err != nil {
			slog.Error("postgres idle pool", "err", err)
			return EX_OSERR
		}
		defer idlePool.Close()
	}

	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, pool, false, false)
	if lerr != nil {
		slog.Error("offline maintenance barrier", "err", lerr)
		return EX_OSERR
	}
	lease.HoldUntilExit()
	store := blob.NewStore(cfg.Storage.Root)
	store.SetGroupWritable(cfg.Storage.GroupWritable)
	if cfg.Production {
		if err := store.CheckPermissions(); err != nil {
			slog.Error("blob store permissions", "err", err)
			return EX_CONFIG
		}
	}

	backend := &imapsess.Backend{
		Pool:              pool,
		IdlePool:          idlePool,
		BlobStore:         store,
		Logger:            slog.Default(),
		StmtTimeout:       mustParseDuration(cfg.Postgres.StatementTimeout, 10*time.Second),
		MaxAppendBytes:    cfg.Limits.MaxAppendBytes,
		MaxSearchResults:  cfg.Limits.MaxSearchResults,
		CommandRatePerSec: cfg.Limits.CommandRatePerSec,
		IdleHeartbeat:     mustParseDuration(cfg.Limits.IdleTimeout, 29*time.Minute),
		PreAuthTimeout:    mustParseDuration(cfg.Limits.PreAuthTimeout, 60*time.Second),
		DeleteArchives:    cfg.Archive.DeleteArchives,
		AuthBudget:        imapsess.NewAuthBudgetMiB(cfg.Limits.AuthVerifyBudgetMiB),
		AuthTiming:        imapsess.NewAuthTimingFloor(mustParseDuration(cfg.Limits.AuthMinDuration, imapsess.DefaultAuthMinDuration)),
		PerIP:             imapsess.NewPerIPLimiter(cfg.Limits.PerIPConnections),
		PerMailbox:        imapsess.NewMailboxSessionLimiter(cfg.Limits.PerMailboxConnections),
		LoginLimiter: imapsess.NewLoginThrottle(
			cfg.Limits.LoginAttempts,
			mustParseDuration(cfg.Limits.LoginWindow, 60*time.Second),
			mustParseDuration(cfg.Limits.LoginCooldown, 60*time.Second),
		),
	}

	if err := imapsess.AuditAuthProfiles(ctx, pool, backend.AuthBudget, slog.Default()); err != nil {
		slog.Error("authentication cost audit", "err", err)
		return EX_CONFIG
	}

	registerBackendMetrics(backend)

	adminSrv, adminErrCh := startAdminListener(cfg, pool)
	imapLn, err := startIMAPListener(ctx, cfg, backend)
	if err != nil {
		slog.Error("IMAP listener failed to start", "err", err)
		shutdownAdmin(adminSrv, cfg)
		return EX_OSERR
	}

	// Never let "no rate limiting" be silent: a zero value legitimately
	// disables each of these limiters, but an operator who set it by accident
	// gets no other signal (RO5X-032).
	if cfg.Limits.LoginAttempts == 0 {
		slog.Warn("login throttling DISABLED (limits.login_attempts = 0); " +
			"credential-spray protection is off")
	}
	if cfg.Limits.CommandRatePerSec == 0 {
		slog.Warn("per-session command rate limiting DISABLED " +
			"(limits.command_rate_per_sec = 0)")
	}
	if cfg.Limits.MaxSearchResults == 0 {
		slog.Warn("SEARCH result cap DISABLED (limits.max_search_results = 0); " +
			"a single SEARCH ALL on a large folder is an unbounded allocation")
	}
	if cfg.Server.DrainTimeoutSec == 0 {
		slog.Warn("graceful drain DISABLED (server.drain_timeout_seconds = 0); " +
			"every session is force-closed at shutdown")
	}

	// Archive sorting's server-side jobs share the query pool; the loop is
	// stopped and waited for before the pool closes.
	archiveCtx, stopArchive := context.WithCancel(ctx)
	archiveDone := startArchiveLoop(archiveCtx, pool, cfg, slog.Default())
	defer func() {
		stopArchive()
		<-archiveDone
	}()

	slog.Info("epistula-imap running",
		"imap_listen", cfg.Server.ListenAddr,
		"admin_listen", cfg.Admin.ListenAddr,
		"production", cfg.Production,
	)

	rc := EX_OK
	select {
	case <-ctx.Done():
		slog.Info("epistula-imap shutting down on signal")
	case err := <-adminErrCh:
		slog.Error("admin listener error", "err", err)
		rc = EX_OSERR
	case err := <-imapLn.errCh:
		slog.Error("IMAP listener error", "err", err)
		rc = EX_OSERR
	}

	// Graceful drain: stop accepting, give live sessions until the drain
	// timeout to finish, then force-close stragglers. Only after the drain
	// is the PG pool torn down (the deferred Close above runs last).
	imapLn.stopAccepting()
	drainSessions(backend, imapLn.forceClose,
		time.Duration(cfg.Server.DrainTimeoutSec)*time.Second)
	shutdownAdmin(adminSrv, cfg)
	return rc
}

// drainSessions waits for every open IMAP session to close, force-closing
// whatever remains when the timeout expires. A zero or negative timeout
// force-closes immediately.
func drainSessions(backend *imapsess.Backend, forceClose func(), timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for backend.ActiveSessions() > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := backend.ActiveSessions(); n > 0 {
		slog.Warn("drain timeout; force-closing remaining sessions", "sessions", n)
		forceClose()
		// Give the force-close a moment to run the session Close hooks
		// before the PG pool goes away.
		settle := time.Now().Add(2 * time.Second)
		for backend.ActiveSessions() > 0 && time.Now().Before(settle) {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// imapListener bundles the running IMAP server's control points: listener
// errors, "stop accepting new connections", and "force-close everything".
type imapListener struct {
	errCh         <-chan error
	stopAccepting func()
	forceClose    func()
}

// advertisedCaps is the capability set sent in the CAPABILITY response.
//
// Only capabilities that are actually implemented belong here — a client
// reads CAPABILITY and decides what to do, so advertising something
// unimplemented is worse than advertising nothing. The converse was the
// defect (RO5X-007): IDLE, LIST-EXTENDED, and SPECIAL-USE were fully
// implemented and documented as shipped, but never advertised, so every MUA
// fell back to polling and the whole pg_notify('mail_arrived') push path —
// plus the dedicated IDLE connection pool and its config key — was dead
// weight in production.
//
// Deliberately NOT listed because the library emits them itself — verified
// on the wire in caps_ro5x007_test.go, which logs the full CAPABILITY line:
//
//	pre-auth:  IMAP4rev2 IMAP4rev1 SASL-IR LITERAL- AUTH=PLAIN
//	post-auth: … UNSELECT ENABLE IDLE UTF8=ACCEPT NAMESPACE UIDPLUS ESEARCH
//	           SEARCHRES LIST-EXTENDED LIST-STATUS MOVE STATUS=SIZE CHILDREN
//	           SPECIAL-USE
//
// So IMAP4rev1, SASL-IR, LITERAL-, AUTH=PLAIN, ENABLE, and UTF8=ACCEPT need no
// entry here; this was checked on the wire rather than assumed (RO5X-007).
// Note the extension set appears only POST-auth; a pre-auth
// CAPABILITY legitimately shows just the login-relevant subset.
//
// Still deliberately absent:
//
//   - COMPRESS=DEFLATE — CRIME-class concerns; no implementation ships.
//   - CONDSTORE / QRESYNC — schema groundwork exists, wire support deferred.
//   - CREATE-SPECIAL-USE — distinct from SPECIAL-USE (RFC 6154). Not
//     advertised, though note the library routes a CREATE USE parameter to
//     Session.Create regardless; Create validates and persists it rather than
//     dropping it silently.
func advertisedCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
		imap.CapNamespace: {},
		imap.CapUIDPlus:   {},
		imap.CapESearch:   {},
		imap.CapMove:      {},

		// Session.Idle is implemented against a real LISTEN mail_arrived
		// consumer with its own connection pool (imapsess/idle.go).
		imap.CapIdle: {},
		// Session.List honours options.SelectSubscribed /
		// options.ReturnSubscribed and emits \Subscribed.
		imap.CapListExtended: {},
		// Session.List emits the folders.special_use attribute.
		imap.CapSpecialUse: {},
	}
}

// startIMAPListener brings up the implicit-TLS listener on cfg.Server.ListenAddr
// and starts the go-imap server. In dev mode (accept_insecure_for_dev) it
// accepts cleartext. With TLS, the certificate is served through a
// certReloader and a SIGHUP handler swaps in renewed keypairs without
// dropping connections.
func startIMAPListener(ctx context.Context, cfg *Config, backend *imapsess.Backend) (*imapListener, error) {
	minVer, err := cfg.MinTLSVersion()
	if err != nil {
		return nil, err
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return backend.NewSessionForNetConn(c.NetConn()), &imapserver.GreetingData{
				PreAuth: false,
			}, nil
		},
		Caps:         advertisedCaps(),
		Logger:       slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		InsecureAuth: cfg.Server.AcceptInsecure,
	})

	// Listener stack, innermost first:
	//   TCP accept → per-IP cap (cheap, pre-TLS) → TLS → eager handshake
	//   with deadline → go-imap Serve.
	// The per-IP cap runs before any TLS work so a capped IP never costs a
	// handshake; its BYE rejection line is therefore written in cleartext
	// (the rejected client sees a handshake failure, which is fine — it
	// was being dropped either way).
	var ln net.Listener
	if cfg.Server.AcceptInsecure && cfg.Server.TLSCert == "" {
		raw, err := net.Listen("tcp", cfg.Server.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", cfg.Server.ListenAddr, err)
		}
		slog.Warn("IMAP listener accepting cleartext (accept_insecure_for_dev=true)")
		ln = imapsess.NewLimitedListener(raw, backend.PerIP, slog.Default())
	} else {
		if cfg.Server.AcceptInsecure {
			slog.Warn("accept_insecure_for_dev=true but tls_cert is set; serving TLS (clear tls_cert for a cleartext dev listener)")
		}
		reloader, err := newCertReloader(cfg.Server.TLSCert, cfg.Server.TLSKey)
		if err != nil {
			return nil, err
		}
		tlsCfg := &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     minVer,
			// AEAD-only allowlist for TLS 1.2 (the explicit opt-in path);
			// Go ignores CipherSuites for TLS 1.3.
			CipherSuites: aeadCipherSuites,
		}
		raw, err := net.Listen("tcp", cfg.Server.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", cfg.Server.ListenAddr, err)
		}
		limited := imapsess.NewLimitedListener(raw, backend.PerIP, slog.Default())
		ln = imapsess.NewTLSHandshakeListener(
			tls.NewListener(limited, tlsCfg),
			mustParseDuration(cfg.Limits.TLSHandshakeTimeout, 10*time.Second),
			slog.Default(),
		)

		// SIGHUP → certificate reload, for Let's Encrypt renewals. A
		// failed reload keeps the current cert and logs at ERROR.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			defer signal.Stop(hup)
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if err := reloader.Reload(); err != nil {
						slog.Error("SIGHUP certificate reload failed; keeping current cert", "err", err)
					} else {
						slog.Info("SIGHUP certificate reloaded", "cert", cfg.Server.TLSCert)
					}
				}
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			errCh <- err
		}
	}()

	return &imapListener{
		errCh:         errCh,
		stopAccepting: func() { _ = ln.Close() },
		forceClose:    func() { _ = srv.Close() },
	}, nil
}

func openPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	if cfg.Postgres.MaxOpenConns > 0 {
		poolCfg.MaxConns = int32(cfg.Postgres.MaxOpenConns)
	}
	if cfg.Postgres.MaxIdleConns > 0 {
		poolCfg.MinConns = int32(cfg.Postgres.MaxIdleConns)
	}
	if d := mustParseDuration(cfg.Postgres.ConnMaxLifetime, 5*time.Minute); d > 0 {
		poolCfg.MaxConnLifetime = d
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	// No eager Ping. pgxpool is lazy and reconnects on demand, so a
	// Postgres that is briefly down at startup — a reboot where the
	// ordering slips, a PG upgrade, a blip to a remote server — must not
	// kill the IMAP daemon. Under rc.d with no restart supervisor it would
	// then stay dead until an operator noticed, which is exactly the
	// outcome CLAUDE.md's documented contract rules out: "The daemon does
	// NOT exit — it keeps the listener up and retries PG in the
	// background." A DSN that fails to parse is still fatal above; that is
	// a genuine config error, not a transient backend (RO5X-010).
	//
	// The session methods already answer NO [UNAVAILABLE] on a DB error, so
	// a connection arriving while PG is down gets a clean protocol
	// response, and /healthz below reports not-ready.
	//
	// This mirrors epistula-database's R-043 storage.Config.SkipPing.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if perr := pool.Ping(probeCtx); perr != nil {
		slog.Warn("postgres not reachable at startup; continuing with a lazy pool",
			"err", perr)
	}
	return pool, nil
}

// openIdlePool builds the IDLE-only LISTEN pool: same DSN, capped at
// idle_conn_pool_size connections, no minimum kept warm.
func openIdlePool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	poolCfg.MaxConns = int32(cfg.Postgres.IdleConns)
	poolCfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

func mustParseDuration(s string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}

func startAdminListener(cfg *Config, pool *pgxpool.Pool) (*http.Server, <-chan error) {
	mux := http.NewServeMux()
	if cfg.Admin.ExposeMetrics {
		mux.Handle("/metrics", metricsHandler())
		// expvar mirrors the "Prometheus + expvar" pattern shared by the
		// other Epistula daemons; same loopback-only exposure rules as /metrics.
		mux.Handle("/debug/vars", expvar.Handler())
	}
	// /health is pure liveness (the process is up and serving). /healthz is
	// readiness (the backend is actually usable), matching epistula-database and
	// epistula-api. Mapping both to the same always-200 handler left monitoring
	// unable to tell "process alive" from "backend usable" (RO5X-010).
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", readyHandler(pool, cfg.Storage.Root))

	srv := &http.Server{
		Addr:         cfg.Admin.ListenAddr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Admin.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Admin.WriteTimeoutSec) * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	return srv, errCh
}

func shutdownAdmin(srv *http.Server, cfg *Config) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Admin.ShutdownTimeoutSec)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("admin shutdown error", "err", err)
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// readyHandler is the readiness probe: a bounded PG ping plus a stat of the
// blob-store root. Copied from epistula-api/serve.go so all three daemons report
// readiness identically. 503 with a plain-text reason on failure, so an
// operator reading a monitoring alert learns which half is broken.
func readyHandler(pool *pgxpool.Pool, storageRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: postgres: %v\n", err)
			return
		}
		fi, err := os.Stat(storageRoot)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: storage_root: %v\n", err)
			return
		}
		if !fi.IsDir() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: storage_root %q is not a directory\n", storageRoot)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	}
}
