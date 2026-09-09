// Command jdix-apiserver is the tenant-facing control plane.
//
// It authenticates API keys, enforces quota, and creates Sandbox objects for
// jdix-controller to act on. It never touches a Pod: exactly one component
// decides which Sandbox owns which Pod, and that is the controller.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/apiserver"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	var (
		addr      = flag.String("addr", ":8000", "listen address")
		dsn       = flag.String("database-url", os.Getenv("DATABASE_URL"), "Postgres connection string; empty uses an in-memory store")
		migrate   = flag.Bool("migrate", true, "apply the schema at start-up (idempotent)")
		devSeed   = flag.Bool("dev-seed", false, "create a demo tenant and print an API key; in-memory store only")
		readyWait = flag.Duration("ready-timeout", 8*time.Second, "how long create waits before returning a pending sandbox")
		logLevel  = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel).With("component", "jdix-apiserver")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store, closeStore, err := openStore(ctx, *dsn, *migrate, *devSeed, log)
	if err != nil {
		log.Error("opening the store", "err", err)
		os.Exit(1)
	}
	defer closeStore()

	kube, reader, err := newCachedClient(ctx, log)
	if err != nil {
		log.Error("connecting to Kubernetes", "err", err)
		os.Exit(1)
	}

	srv := &apiserver.Server{
		Client:       kube,
		APIReader:    reader,
		Store:        store,
		Auth:         apiserver.NewAuthenticator(store),
		Log:          log,
		ReadyTimeout: *readyWait,
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// Generous: a create legitimately waits for a cold start before it can
		// answer, and cutting it off would turn a slow success into a failure.
		WriteTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Error("server stopped", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func openStore(ctx context.Context, dsn string, migrate, devSeed bool, log *slog.Logger) (apiserver.Store, func(), error) {
	if dsn == "" {
		// Nothing survives a restart, so this is for development only — and it
		// says so rather than quietly behaving like a database.
		log.Warn("no --database-url: using an in-memory store; all state is lost on restart")
		mem := apiserver.NewMemStore()
		if devSeed {
			seed(mem, log)
		}
		return mem, func() {}, nil
	}
	pg, err := apiserver.NewPGStore(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	if migrate {
		if err := pg.Migrate(ctx, schemaSQL); err != nil {
			pg.Close()
			return nil, nil, fmt.Errorf("applying the schema: %w", err)
		}
		log.Info("schema applied")
	}
	if devSeed {
		log.Warn("--dev-seed is ignored with a real database; provision tenants and keys properly")
	}
	return pg, pg.Close, nil
}

// seed makes a local run usable in one command by minting a key and printing it.
func seed(mem *apiserver.MemStore, log *slog.Logger) {
	mem.AddTenant(&apiserver.Tenant{
		ID: "t_dev", Name: "dev", Namespace: "tenant-dev", Status: "active", CreatedAt: time.Now(),
	})
	gen, err := apiserver.GenerateKey()
	if err != nil {
		log.Error("minting the demo key", "err", err)
		return
	}
	mem.AddKey(&apiserver.APIKey{
		KeyID: gen.KeyID, TenantID: "t_dev", Name: "dev", SecretHash: gen.Hash,
		Scopes: []apiserver.Scope{apiserver.ScopeAdmin}, CreatedAt: time.Now(),
	})
	// Printed once, exactly as a real key would be: there is no way to recover
	// it afterwards, and pretending otherwise teaches the wrong habit.
	fmt.Fprintf(os.Stderr, "\n  dev API key (shown once): %s\n\n", gen.Plaintext)
}

// newCachedClient returns a client backed by informers.
//
// The create path polls for readiness, and polling an informer cache costs
// nothing. Polling the API server directly would put tens of requests per
// create onto the control plane, which is a poor way to spend its capacity.
func newCachedClient(ctx context.Context, log *slog.Logger) (client.Client, client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := sbxv1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	cl, err := cluster.New(ctrl.GetConfigOrDie(), func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		return nil, nil, err
	}
	go func() {
		if err := cl.Start(ctx); err != nil {
			log.Error("cluster cache stopped", "err", err)
		}
	}()
	if !cl.GetCache().WaitForCacheSync(ctx) {
		return nil, nil, errors.New("informer cache did not sync")
	}
	// The second reader bypasses the cache. It is only consulted when the cache
	// claims an object is missing, which straight after a create can only mean
	// it has not caught up yet.
	return cl.GetClient(), cl.GetAPIReader(), nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
