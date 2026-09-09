// Checks whether the controller can actually reach and authenticate to a
// sandbox Pod, using the same client the controller does.
//
// Worth reaching for when a bind fails and the reason is not obvious. It
// separates three things a 401 cannot tell apart: the Pod is unreachable, the
// Pod carries no credential, and the credential is not arriving.
//
//	go run ./hack/dev/proxyprobe <namespace> <pod> [token]
//
// With no token it reads the Pod's own, which is where the controller gets it.
package main

import (
	"context"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/controller"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: proxyprobe <namespace> <pod> [token]")
		fmt.Fprintln(os.Stderr, "  with no token, the Pod's own is read from its spec")
		os.Exit(2)
	}
	ns, name := os.Args[1], os.Args[2]
	token := ""
	if len(os.Args) > 3 {
		token = os.Args[3]
	} else {
		var err error
		if token, err = podToken(ns, name); err != nil {
			fmt.Fprintln(os.Stderr, "reading the Pod's token:", err)
			os.Exit(1)
		}
	}
	c, err := controller.NewAPIProxyExecdClient(ctrl.GetConfigOrDie())
	if err != nil {
		fmt.Println("client:", err)
		os.Exit(1)
	}
	pod := controller.PodRef{Namespace: ns, Name: name, Token: token}
	ctx := context.Background()

	tier, reason, err := c.Probe(ctx, pod)
	if err != nil {
		fmt.Printf("  probe (unauthenticated) -> FAILED: %v\n", err)
		fmt.Println("\n  The Pod is not reachable at all; the credential is not the problem yet.")
		os.Exit(1)
	}
	fmt.Printf("  probe (unauthenticated) -> ok, tier=%v (%s)\n", tier, reason)

	// Binding leaves the Pod claimed inside execd even though its labels still
	// say idle, so it is released again immediately.
	_, err = c.Bind(ctx, pod, api.BindRequest{
		SandboxID: "sbx-proxyprobe", Tenant: "probe", Token: "sbt-probe", TTLSeconds: 60,
		Filesystem: api.FilesystemSpec{Workspace: api.Workspace{Path: "/workspace"}},
	})
	if err != nil {
		fmt.Printf("  bind  (authenticated)   -> FAILED: %v\n", err)
		fmt.Println("\n  A 401 here with a working probe means the credential is not arriving.")
		fmt.Println("  The API server strips Authorization from proxied requests; the token")
		fmt.Println("  travels in " + api.ControlTokenHeader + " instead.")
		os.Exit(1)
	}
	fmt.Println("  bind  (authenticated)   -> ok")
	if err := c.Unbind(ctx, pod); err != nil {
		fmt.Printf("  note: could not release the Pod (%v); delete it manually\n", err)
	} else {
		fmt.Println("  released")
	}
}

// podToken reads the credential the controller would use.
func podToken(ns, name string) (string, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return controller.PodToken(pod), nil
}
