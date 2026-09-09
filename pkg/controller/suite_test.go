package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"jdix.io/sandbox/pkg/api"
	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

const testNS = "tenant-abc"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := sbxv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&sbxv1.Sandbox{}, &sbxv1.SandboxTemplate{}, &sbxv1.SandboxPool{}).
		Build()
}

// fakeBinder records what execd would have been told, and can be made to fail
// the way a rejected filesystem spec does.
type fakeBinder struct {
	calls    []api.BindRequest
	unbinds  []string
	err      error
	tier     bwrap.Tier
	probeErr error
	probes   []string
}

func (f *fakeBinder) Bind(_ context.Context, podIP string, req api.BindRequest) (api.BindResponse, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return api.BindResponse{}, f.err
	}
	tier := f.tier
	if tier == "" {
		tier = bwrap.TierUserns
	}
	return api.BindResponse{SandboxID: req.SandboxID, IsolationTier: string(tier)}, nil
}

func (f *fakeBinder) Unbind(_ context.Context, podIP string) error {
	f.unbinds = append(f.unbinds, podIP)
	return nil
}

func (f *fakeBinder) Probe(_ context.Context, podIP string) (bwrap.Tier, string, error) {
	f.probes = append(f.probes, podIP)
	if f.probeErr != nil {
		return "", "", f.probeErr
	}
	tier := f.tier
	if tier == "" {
		tier = bwrap.TierUserns
	}
	return tier, "test", nil
}

func approvedTemplate(name string, mods ...func(*sbxv1.SandboxTemplate)) *sbxv1.SandboxTemplate {
	t := &sbxv1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: sbxv1.SandboxTemplateSpec{
			MinIsolationTier:  sbxv1.TierUserns,
			DefaultTTLSeconds: 1800,
			MaxTTLSeconds:     14400,
			Image:             sbxv1.ImageSpec{Ref: "registry.internal/jdix/py312@sha256:" + repeat64('a')},
		},
		Status: sbxv1.SandboxTemplateStatus{Admission: sbxv1.AdmissionApproved},
	}
	for _, m := range mods {
		m(t)
	}
	t.Status.Hash = TemplateHash(t)
	return t
}

func repeat64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func warmPod(name, template, hash, tier string, ready bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels: map[string]string{
				sbxv1.LabelTemplate:     template,
				sbxv1.LabelTemplateHash: hash,
				sbxv1.LabelState:        sbxv1.StateIdle,
				sbxv1.LabelManagedBy:    sbxv1.ManagedBy,
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec:   corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"},
	}
	if tier != "" {
		p.Labels[sbxv1.LabelIsolationTier] = tier
	}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func newSandbox(name, template string, mods ...func(*sbxv1.Sandbox)) *sbxv1.Sandbox {
	s := &sbxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels:    map[string]string{sbxv1.LabelTenant: "tenant-abc"},
		},
		Spec: sbxv1.SandboxSpec{TemplateRef: template, TTLSeconds: 600},
	}
	for _, m := range mods {
		m(s)
	}
	return s
}

func newSandboxReconciler(t *testing.T, c client.Client, b *fakeBinder) *SandboxReconciler {
	t.Helper()
	return &SandboxReconciler{
		Client:      c,
		Scheme:      testScheme(t),
		Layout:      bwrap.DefaultLayout(),
		Platform:    PlatformImage{Ref: "registry.internal/jdix/platform@sha256:" + repeat64('b')},
		Binder:      b,
		Prober:      b,
		TokenSecret: "jdix-control-token",
		EndpointFor: func(id string) string { return "https://" + id + ".sbx.example.com" },
		NewToken:    func() string { return "sbt_deterministic" },
	}
}

func reconcileSandbox(t *testing.T, r *SandboxReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// drive runs the reconciler until the sandbox reaches a terminal-ish phase or
// the step budget runs out, mimicking the requeue loop the manager would drive.
func drive(t *testing.T, r *SandboxReconciler, c client.Client, name string, steps int) *sbxv1.Sandbox {
	t.Helper()
	var sbx sbxv1.Sandbox
	for i := 0; i < steps; i++ {
		reconcileSandbox(t, r, name)
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &sbx); err != nil {
			t.Fatal(err)
		}
		switch sbx.Status.Phase {
		case sbxv1.PhaseRunning, sbxv1.PhaseFailed, sbxv1.PhaseExpired:
			return &sbx
		}
	}
	return &sbx
}

func mustGetPod(t *testing.T, c client.Client, name string) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func listPods(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var l corev1.PodList
	if err := c.List(context.Background(), &l, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

var errBindRejected = errors.New(`execd: bind_failed: filesystem.mounts[0].path "/opt/jdix": overlaps platform path /opt/jdix`)
