// Command jdix-gateway publishes sandboxes on the public network.
//
// It resolves a hostname to a Pod IP and proxies straight there. Sandbox Pods
// deliberately have no Service: creating one per sandbox would add a Service
// and Endpoints object per tenant request, and endpoint propagation alone can
// take longer than the entire warm-pool bind it would be sitting behind.
package main

import (
	"context"
	"errors"
	"flag"
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
	"jdix.io/sandbox/pkg/gateway"
)

func main() {
	var (
		addr     = flag.String("addr", ":8080", "listen address")
		suffix   = flag.String("suffix", "sbx.example.com", "wildcard domain sandboxes are published under")
		cacheTTL = flag.Duration("resolve-cache-ttl", 2*time.Second, "how long a hostname resolution is reused")
		logLevel = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel).With("component", "jdix-gateway")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	reader, err := startCache(ctx, log)
	if err != nil {
		log.Error("starting the informer cache", "err", err)
		os.Exit(1)
	}

	resolver := gateway.NewCachedResolver(reader)
	resolver.TTL = *cacheTTL

	srv := &gateway.Server{Resolver: resolver, Suffix: *suffix, Log: log}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: PTY sessions and exec streams are long-lived by
		// design, and cutting them off after a fixed period would look to a
		// user exactly like the sandbox crashing.
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "suffix", *suffix)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// startCache brings up an informer over Sandbox objects and indexes them by
// name, so resolving a hostname is a map lookup rather than an API call.
func startCache(ctx context.Context, log *slog.Logger) (client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := sbxv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	cl, err := cluster.New(ctrl.GetConfigOrDie(), func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		return nil, err
	}
	// Sandbox names are unique across the cluster but Get needs a namespace, so
	// the lookup is a List against this index instead.
	if err := cl.GetFieldIndexer().IndexField(ctx, &sbxv1.Sandbox{}, "metadata.name",
		func(o client.Object) []string { return []string{o.GetName()} }); err != nil {
		return nil, err
	}
	go func() {
		if err := cl.Start(ctx); err != nil {
			log.Error("cluster cache stopped", "err", err)
		}
	}()
	if !cl.GetCache().WaitForCacheSync(ctx) {
		return nil, errors.New("informer cache did not sync")
	}
	return cl.GetClient(), nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
