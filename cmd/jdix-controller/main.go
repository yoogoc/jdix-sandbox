// Command jdix-controller reconciles Sandbox, SandboxTemplate and SandboxPool.
//
// It is the only writer of a Pod's state label. Binding lives here rather than
// in the API server so there is exactly one component deciding which Sandbox
// owns which Pod.
package main

import (
	"flag"
	"fmt"
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
	"jdix.io/sandbox/pkg/gateway"
)

var scheme = runtime.NewScheme()

func init() {
	must(clientgoscheme.AddToScheme(scheme))
	must(sbxv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr   = flag.String("metrics-bind-address", ":8080", "metrics listen address")
		probeAddr     = flag.String("health-probe-bind-address", ":8081", "health probe listen address")
		leaderElect   = flag.Bool("leader-elect", true, "elect a leader before reconciling")
		platformImage = flag.String("platform-image", "", "image carrying execd, jdix-init and bubblewrap")
		endpointMode  = flag.String("endpoint-mode", "path",
			"how sandbox URLs are formed: 'path' publishes {base}/s/{id}; 'host' publishes {id}.{suffix}. Must match the gateway's --route-mode")
		endpointSuffix = flag.String("endpoint-suffix", "sbx.example.com", "wildcard domain, for --endpoint-mode=host")
		endpointBase   = flag.String("endpoint-base", "", "public origin, for --endpoint-mode=path, e.g. https://api.example.com")
		endpointPrefix = flag.String("endpoint-path-prefix", gateway.DefaultPathPrefix, "routing prefix, for --endpoint-mode=path; must match the gateway's --path-prefix")
		registries     = flag.String("allowed-registries", "", "comma-separated registry allowlist for tenant images")
		resolveImages  = flag.Bool("resolve-images", true,
			"resolve a template's image tag to a digest by asking the registry; disable for an air-gapped install, where references must already be pinned")
		maxImageBytes = flag.Int64("max-image-bytes", 5<<30, "reject template images larger than this")
		transport     = flag.String("execd-transport", "direct",
			"how to reach execd: 'direct' dials the Pod IP; 'apiserver-proxy' goes through the Kubernetes API server's pod proxy (for running this controller outside the cluster)")
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
	endpoints, err := newEndpointRouter(*endpointMode, *endpointSuffix, *endpointBase, *endpointPrefix)
	if err != nil {
		log.Error(err, "configuring sandbox URLs")
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

	// Sandbox Pods have no Service, so the direct transport needs a route to the
	// Pod network. A controller running outside the cluster usually has none —
	// and port-forward cannot substitute, because a bind targets whichever Pod
	// the pool hands over, which is not known ahead of time. The proxy transport
	// borrows the API server's reach instead.
	var (
		binder controller.Binder
		prober controller.Prober
	)
	switch *transport {
	case "direct":
		c := controller.NewHTTPExecdClient()
		binder, prober = c, c
	case "apiserver-proxy":
		c, err := controller.NewAPIProxyExecdClient(ctrl.GetConfigOrDie())
		if err != nil {
			log.Error(err, "building the API-proxy transport")
			os.Exit(1)
		}
		binder, prober = c, c
		log.Info("reaching execd through the API server's pod proxy",
			"note", "every bind becomes API server load; do not use this in production")
	default:
		log.Error(nil, "unknown --execd-transport", "value", *transport,
			"valid", "direct, apiserver-proxy")
		os.Exit(1)
	}

	platform := controller.PlatformImage{Ref: *platformImage}
	layout := bwrap.DefaultLayout()

	// Templates are written with a tag, because that is what people write. The
	// resolver turns it into a digest once; everything downstream uses the
	// digest. Without one, only already-pinned references are accepted.
	var resolver controller.ImageResolver
	if *resolveImages {
		rr, err := controller.NewRegistryResolver(ctrl.GetConfigOrDie())
		if err != nil {
			log.Error(err, "building the image resolver")
			os.Exit(1)
		}
		resolver = rr
	} else {
		log.Info("image resolution disabled; spec.image.ref must be pinned by digest")
	}

	if err := (&controller.TemplateReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Platform:          platform,
		Layout:            layout,
		AllowedRegistries: splitList(*registries),
		Resolver:          resolver,
		MaxImageBytes:     *maxImageBytes,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the template reconciler")
		os.Exit(1)
	}

	if err := (&controller.PoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Layout:   layout,
		Platform: platform,
		Prober:   prober,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the pool reconciler")
		os.Exit(1)
	}

	if err := (&controller.SandboxReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Layout:      layout,
		Platform:    platform,
		Binder:      binder,
		Prober:      prober,
		EndpointFor: endpoints.EndpointURL,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "wiring the sandbox reconciler")
		os.Exit(1)
	}

	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	log.Info("starting", "platformImage", *platformImage, "endpointMode", *endpointMode,
		"sampleEndpoint", endpoints.EndpointURL("sbx-example"), "execdTransport", *transport)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

// newEndpointRouter renders the URLs the gateway has to accept.
//
// Both ends build the same type from their own flags rather than each
// formatting a string, because a controller that published one shape while the
// gateway parsed another would produce sandboxes that look healthy in every
// status field and 404 for the tenant.
func newEndpointRouter(mode, suffix, base, prefix string) (gateway.Router, error) {
	switch mode {
	case "path":
		if base == "" {
			return nil, fmt.Errorf("--endpoint-base is required with --endpoint-mode=path: " +
				"a sandbox URL has to be absolute, and nothing here can guess the public origin")
		}
		return gateway.PathRouter{Prefix: prefix, Base: strings.TrimSuffix(base, "/")}, nil
	case "host":
		return gateway.HostRouter{Suffix: suffix, Scheme: "https"}, nil
	default:
		return nil, fmt.Errorf("unknown --endpoint-mode %q: want path or host", mode)
	}
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
