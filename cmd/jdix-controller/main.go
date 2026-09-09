// Command jdix-controller reconciles Sandbox, SandboxTemplate and SandboxPool.
//
// It is the only writer of a Pod's state label. Binding lives here rather than
// in the API server so there is exactly one component deciding which Sandbox
// owns which Pod.
package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
	"jdix.io/sandbox/pkg/controller"
)

var scheme = runtime.NewScheme()

func init() {
	must(clientgoscheme.AddToScheme(scheme))
	must(sbxv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    = flag.String("metrics-bind-address", ":8080", "metrics listen address")
		probeAddr      = flag.String("health-probe-bind-address", ":8081", "health probe listen address")
		leaderElect    = flag.Bool("leader-elect", true, "elect a leader before reconciling")
		platformImage  = flag.String("platform-image", "", "image carrying execd, jdix-init and bubblewrap")
		tokenSecret    = flag.String("control-token-secret", "jdix-control-token", "Secret holding execd's control-plane token")
		controlToken   = flag.String("control-token", "", "control-plane bearer token; prefer --control-token-file")
		tokenFile      = flag.String("control-token-file", "", "file holding the control-plane bearer token")
		endpointSuffix = flag.String("endpoint-suffix", "sbx.example.com", "wildcard domain sandboxes are published under")
		registries     = flag.String("allowed-registries", "", "comma-separated registry allowlist for tenant images")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	if *platformImage == "" {
		log.Error(nil, "--platform-image is required: without it a sandbox Pod has no execd to run")
		os.Exit(1)
	}
	token, err := resolveToken(*controlToken, *tokenFile)
	if err != nil {
		log.Error(err, "reading the control-plane token")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "jdix-controller.sandbox.jdix.io",
		// Losing the lease should stop this process promptly: two controllers
		// binding Pods at once is the one thing the design rules out.
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptr(20 * time.Second),
	})
	if err != nil {
		log.Error(err, "starting the manager")
		os.Exit(1)
	}

	execd := controller.NewHTTPExecdClient(token)
	platform := controller.PlatformImage{Ref: *platformImage}
	layout := bwrap.DefaultLayout()

	if err := (&controller.TemplateReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		AllowedRegistries: splitList(*registries),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the template reconciler")
		os.Exit(1)
	}

	if err := (&controller.PoolReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Layout:      layout,
		Platform:    platform,
		Prober:      execd,
		TokenSecret: *tokenSecret,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the pool reconciler")
		os.Exit(1)
	}

	if err := (&controller.SandboxReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Layout:      layout,
		Platform:    platform,
		Binder:      execd,
		Prober:      execd,
		TokenSecret: *tokenSecret,
		EndpointFor: func(id string) string { return "https://" + id + "." + *endpointSuffix },
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the sandbox reconciler")
		os.Exit(1)
	}

	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	log.Info("starting", "platformImage", *platformImage, "endpointSuffix", *endpointSuffix)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

func resolveToken(inline, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return inline, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func must(err error) {
	if err != nil {
		panic(err)
	}
}
