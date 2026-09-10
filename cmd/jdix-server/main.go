// Command jdix-server is the tenant-facing control plane and the public data
// plane, in one binary. --role picks which of the two it serves.
//
// They are separate listeners even when one process runs both, because the
// traffic has nothing in common. A control-plane request is a short JSON
// transaction that must not be allowed to hang; a data-plane request is a PTY
// session that must not be cut off. One http.Server carries one WriteTimeout,
// so each gets its own — along with its own port, which is what lets an
// operator put them behind different Services, Ingresses and NetworkPolicies
// without splitting the process.
//
// Running both in one process couples their failure domains: restarting to
// change a quota rule drops every live PTY session. --role exists so that is a
// deployment decision rather than an architectural one. Deploy one Pod with
// --role=all, or two Deployments of the same image with --role=server and
// --role=gateway, and change your mind later without touching any code.
//
// Neither role ever touches a Pod's state label. Exactly one component decides
// which Sandbox owns which Pod, and that is jdix-controller.
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
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/apiserver"
	"jdix.io/sandbox/pkg/gateway"
)

//go:embed schema.sql
var schemaSQL string

// role says which listeners this process brings up.
type role struct{ control, data bool }

func parseRole(s string) (role, error) {
	switch s {
	case "all":
		return role{control: true, data: true}, nil
	case "server":
		return role{control: true}, nil
	case "gateway":
		return role{data: true}, nil
	default:
		return role{}, fmt.Errorf("unknown --role %q: want all, server or gateway", s)
	}
}

func main() {
	var (
		roleFlag = flag.String("role", "all",
			"which listeners to bring up: 'server' is the tenant API, 'gateway' is the sandbox data plane, 'all' is both")
		logLevel = flag.String("log-level", "info", "debug|info|warn|error")

		// Control plane, for --role=server and --role=all.
		addr      = flag.String("addr", ":8000", "control-plane listen address")
		dsn       = flag.String("database-url", os.Getenv("DATABASE_URL"), "Postgres connection string; empty uses an in-memory store")
		migrate   = flag.Bool("migrate", true, "apply the schema at start-up (idempotent)")
		devSeed   = flag.Bool("dev-seed", false, "create a demo tenant and print an API key; in-memory store only")
		readyWait = flag.Duration("ready-timeout", 8*time.Second, "how long create waits before returning a pending sandbox")

		// Data plane, for --role=gateway and --role=all.
		dataAddr  = flag.String("data-addr", ":8090", "data-plane listen address")
		routeMode = flag.String("route-mode", "path",
			"how a request names its sandbox: 'path' serves /s/{id}/... under one hostname; 'host' serves {id}.{suffix}; 'both' accepts either and hands out path URLs")
		suffix     = flag.String("suffix", "sbx.example.com", "wildcard domain, for host routing")
		pathPrefix = flag.String("path-prefix", gateway.DefaultPathPrefix, "routing prefix, for path routing; must stay disjoint from the control plane's /v1/")
		publicBase = flag.String("public-base", "",
			"public origin URLs are rendered against, e.g. https://api.example.com; empty derives it from each request, which is right unless something upstream rewrites Host")
		cacheTTL         = flag.Duration("resolve-cache-ttl", 2*time.Second, "how long a sandbox lookup is reused")
		sandboxTransport = flag.String("sandbox-transport", "direct",
			"how to reach a sandbox Pod: 'direct' dials its IP; 'apiserver-proxy' goes through the Kubernetes API server's pod proxy (for running this outside the cluster)")
	)
	flag.Parse()

	log := newLogger(*logLevel).With("component", "jdix-server")
	r, err := parseRole(*roleFlag)
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}

	// Every flag is validated before anything opens a socket. With two roles
	// worth of flags on one binary there are more ways to mistype one, and
	// finding out after a connection to the API server and a database migration
	// is a poor way to learn about it.
	resolver := &lateResolver{}
	router, err := newRouter(*routeMode, *suffix, *pathPrefix, *publicBase, resolver)
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}
	transportKind, err := parseTransport(*sandboxTransport)
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The gateway resolves a sandbox by name across every namespace, which
	// needs an index; the control plane always knows the namespace and does
	// not. Building it only when it is used keeps the cache honest about what
	// this process is for.
	cfg := ctrl.GetConfigOrDie()
	kube, reader, err := startCluster(ctx, cfg, log, r.data)
	if err != nil {
		log.Error("connecting to Kubernetes", "err", err)
		os.Exit(1)
	}

	var listeners []*listener

	if r.control {
		store, closeStore, err := openStore(ctx, *dsn, *migrate, *devSeed, log)
		if err != nil {
			log.Error("opening the store", "err", err)
			os.Exit(1)
		}
		defer closeStore()

		srv := &apiserver.Server{
			Client:       kube,
			APIReader:    reader,
			Store:        store,
			Auth:         apiserver.NewAuthenticator(store),
			Log:          log,
			ReadyTimeout: *readyWait,
		}
		listeners = append(listeners, &listener{
			name:  "control-plane",
			grace: 15 * time.Second,
			server: &http.Server{
				Addr:              *addr,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				// Generous: a create legitimately waits for a cold start before
				// it can answer, and cutting it off would turn a slow success
				// into a failure.
				WriteTimeout: 60 * time.Second,
			},
		})
	}

	if r.data {
		cached := gateway.NewCachedResolver(kube)
		cached.TTL = *cacheTTL
		resolver.Resolver = cached

		transport, err := newTransport(transportKind, cfg, log)
		if err != nil {
			log.Error("building the sandbox transport", "err", err)
			os.Exit(1)
		}

		listeners = append(listeners, &listener{
			name: "data-plane",
			// A PTY session ends when someone stops typing into it, so draining
			// is worth more here than promptness.
			grace: 60 * time.Second,
			server: &http.Server{
				Addr: *dataAddr,
				Handler: (&gateway.Server{
					Resolver: cached, Router: router, Transport: transport, Log: log,
				}).Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				// No write timeout: PTY sessions and exec streams are long-lived
				// by design, and cutting them off after a fixed period would look
				// to a user exactly like the sandbox crashing.
			},
		})
	}

	log.Info("starting", "role", *roleFlag, "routeMode", *routeMode, "sandboxTransport", *sandboxTransport)
	if err := serve(ctx, log, listeners); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

// transportKind names a way of reaching a sandbox Pod. It is parsed before
// anything opens a socket and turned into a Transport afterwards, because
// building one needs a REST config and rejecting a typo should not.
type transportKind string

const (
	transportDirect   transportKind = "direct"
	transportAPIProxy transportKind = "apiserver-proxy"
)

func parseTransport(s string) (transportKind, error) {
	switch transportKind(s) {
	case transportDirect, transportAPIProxy:
		return transportKind(s), nil
	default:
		return "", fmt.Errorf("unknown --sandbox-transport %q: want direct or apiserver-proxy", s)
	}
}

func newTransport(kind transportKind, cfg *rest.Config, log *slog.Logger) (gateway.Transport, error) {
	if kind == transportDirect {
		return gateway.DirectTransport{}, nil
	}
	t, err := gateway.NewAPIProxyTransport(cfg)
	if err != nil {
		return nil, err
	}
	// Louder than the controller's equivalent warning, and deliberately so.
	// There, one bind is one proxied request; here every byte of every exec,
	// PTY session and file transfer becomes API server traffic.
	log.Warn("reaching sandboxes through the API server's pod proxy",
		"note", "all data-plane traffic becomes API server load; this is for debugging from outside the cluster, not for production")
	return t, nil
}

// lateResolver stands in for the informer-backed resolver while the routing
// configuration is being checked, and delegates to it once it exists. Only host
// routing consults a resolver, and only while serving a request, by which time
// this has been filled in.
type lateResolver struct{ gateway.Resolver }

// listener is one HTTP server and how long it may take to drain.
type listener struct {
	name   string
	grace  time.Duration
	server *http.Server
}

// serve runs every listener until one fails or the context is cancelled, then
// shuts them all down.
//
// One failing listener stops the process rather than leaving the other half
// serving. A jdix-server that answers the control plane but silently stopped
// proxying sandboxes is worse than one that is plainly down: it looks healthy
// to everything that watches it.
func serve(ctx context.Context, log *slog.Logger, listeners []*listener) error {
	if len(listeners) == 0 {
		return errors.New("no listeners for this role")
	}
	errCh := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			log.Info("listening", "listener", l.name, "addr", l.server.Addr)
			if err := l.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", l.name, err)
				return
			}
			errCh <- nil
		}()
	}

	var failure error
	select {
	case err := <-errCh:
		failure = err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	done := make(chan struct{}, len(listeners))
	for _, l := range listeners {
		go func() {
			defer func() { done <- struct{}{} }()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), l.grace)
			defer cancel()
			if err := l.server.Shutdown(shutdownCtx); err != nil {
				log.Warn("listener did not drain", "listener", l.name, "err", err)
			}
		}()
	}
	for range listeners {
		<-done
	}
	return failure
}

// newRouter builds the routing scheme from flags.
//
// The same construction runs in jdix-controller, which renders the URLs this
// gateway has to accept. The two must agree, which is why the rendering lives
// on the Router rather than in a fmt.Sprintf at each end.
func newRouter(mode, suffix, prefix, base string, res gateway.Resolver) (gateway.Router, error) {
	host := gateway.HostRouter{Suffix: suffix, Resolver: res}
	path := gateway.PathRouter{Prefix: prefix, Base: base}
	switch mode {
	case "path":
		return path, nil
	case "host":
		return host, nil
	case "both":
		// Path first, so a gateway accepting both hands out the new scheme.
		// That is what migrating to it consists of.
		return gateway.ChainRouter{path, host}, nil
	default:
		return nil, fmt.Errorf("unknown --route-mode %q: want path, host or both", mode)
	}
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

// startCluster brings up the informer cache both roles read from.
//
// One cache serves both: the control plane polls it for readiness after a
// create, and the gateway resolves hostnames against it. Polling an informer
// costs nothing, where polling the API server directly would put tens of
// requests per create onto the control plane.
func startCluster(ctx context.Context, cfg *rest.Config, log *slog.Logger, withNameIndex bool) (client.Client, client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := sbxv1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	cl, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		return nil, nil, err
	}
	if withNameIndex {
		// Sandbox names are unique across the cluster but Get needs a
		// namespace, so the gateway's lookup is a List against this index.
		if err := cl.GetFieldIndexer().IndexField(ctx, &sbxv1.Sandbox{}, "metadata.name",
			func(o client.Object) []string { return []string{o.GetName()} }); err != nil {
			return nil, nil, err
		}
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
