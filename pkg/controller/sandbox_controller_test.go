package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

func TestSandboxClaimsAWarmPod(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, pod, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %q reason %q", got.Status.Phase, got.Status.Reason)
	}
	if got.Status.ColdStart {
		t.Error("claiming a warm Pod must not be reported as a cold start")
	}
	if got.Status.PodName != "warm-1" {
		t.Errorf("bound to %q, expected the warm Pod", got.Status.PodName)
	}
	if got.Status.Endpoint == "" || got.Status.ExpiresAt == nil {
		t.Errorf("status is incomplete: %+v", got.Status)
	}

	claimed := mustGetPod(t, c, "warm-1")
	if claimed.Labels[sbxv1.LabelState] != sbxv1.StateBound {
		t.Errorf("pod state is %q, want bound", claimed.Labels[sbxv1.LabelState])
	}
	if claimed.Labels[sbxv1.LabelSandboxID] != "sbx-1" {
		t.Errorf("pod is not stamped with the sandbox id: %v", claimed.Labels)
	}
	// Ownership must move from the pool to the Sandbox, or the pool controller
	// would keep counting a Pod that now belongs to a tenant.
	owner := metav1.GetControllerOf(claimed)
	if owner == nil || owner.Kind != "Sandbox" || owner.Name != "sbx-1" {
		t.Errorf("pod owner is %+v, want the Sandbox", owner)
	}

	if len(b.calls) != 1 {
		t.Fatalf("expected exactly one bind call, got %d", len(b.calls))
	}
	call := b.calls[0]
	if call.Token == "" || call.TTLSeconds != 600 || call.SandboxID != "sbx-1" {
		t.Errorf("bind request is wrong: %+v", call)
	}
	if call.Filesystem.Workspace.Path != "/workspace" {
		t.Errorf("workspace default not applied: %q", call.Filesystem.Workspace.Path)
	}
}

func TestSandboxColdStartsWhenThePoolIsEmpty(t *testing.T) {
	tpl := approvedTemplate("py312")
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	// The first pass only installs the finalizer; the second acquires a Pod.
	step(t, r, "sbx-1", 2)
	pods := listPods(t, c)
	if len(pods) != 1 {
		t.Fatalf("expected one Pod to be created, got %d", len(pods))
	}
	created := pods[0]
	if created.Labels[sbxv1.LabelState] != sbxv1.StateBound {
		t.Error("a cold-started Pod belongs to its sandbox immediately")
	}

	// Simulate the kubelet bringing it up.
	created.Status.Phase = corev1.PodRunning
	created.Status.PodIP = "10.0.0.9"
	created.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), &created); err != nil {
		t.Fatal(err)
	}

	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %q reason %q", got.Status.Phase, got.Status.Reason)
	}
	if !got.Status.ColdStart {
		t.Error("ColdStart must be set: it is the clearest signal that a pool is undersized")
	}
	// A cold Pod has never been measured, so the bind path must measure it.
	if len(b.probes) == 0 {
		t.Error("an unmeasured Pod must be probed before it runs anything")
	}
}

func TestSandboxIgnoresWarmPodsBelowTheTierFloor(t *testing.T) {
	tpl := approvedTemplate("py312") // requires userns
	weak := warmPod("weak-1", "py312", tpl.Status.Hash, string(bwrap.TierChroot), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, weak, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	step(t, r, "sbx-1", 2)

	if p := mustGetPod(t, c, "weak-1"); p.Labels[sbxv1.LabelState] != sbxv1.StateIdle {
		t.Fatal("a Pod below the template's isolation floor must not be claimed")
	}
	if len(listPods(t, c)) != 2 {
		t.Fatal("expected a cold start alongside the unusable warm Pod")
	}
}

func TestSandboxSkipsUnmeasuredAndUnreadyWarmPods(t *testing.T) {
	tpl := approvedTemplate("py312")
	unmeasured := warmPod("unmeasured", "py312", tpl.Status.Hash, "", true)
	notReady := warmPod("not-ready", "py312", tpl.Status.Hash, string(bwrap.TierUserns), false)
	staleHash := warmPod("stale", "py312", "differenthash", string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, unmeasured, notReady, staleHash, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	step(t, r, "sbx-1", 2)

	for _, name := range []string{"unmeasured", "not-ready", "stale"} {
		if p := mustGetPod(t, c, name); p.Labels[sbxv1.LabelState] != sbxv1.StateIdle {
			t.Errorf("%s was claimed but should have been skipped", name)
		}
	}
}

func TestSandboxRefusesANodeThatCannotMeetTheFloor(t *testing.T) {
	tpl := approvedTemplate("py312")
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, sbx)
	// The node turns out to only manage chroot once it is measured.
	b := &fakeBinder{tier: bwrap.TierChroot}
	r := newSandboxReconciler(t, c, b)

	step(t, r, "sbx-1", 2)
	pods := listPods(t, c)
	if len(pods) != 1 {
		t.Fatalf("expected a cold-started Pod, got %d", len(pods))
	}
	pod := pods[0]
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.9"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}

	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseFailed {
		t.Fatalf("phase %q; running with weaker isolation than promised is not an option", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Reason, "IsolationUnavailable") {
		t.Errorf("reason should name the problem: %q", got.Status.Reason)
	}
	if len(b.calls) != 0 {
		t.Error("nothing may be bound on a node that fails the floor")
	}
	if len(listPods(t, c)) != 0 {
		t.Error("the unusable Pod should have been deleted")
	}
}

func TestSandboxSurfacesTheReasonABindWasRejected(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, pod, sbx)
	b := &fakeBinder{err: errBindRejected}
	r := newSandboxReconciler(t, c, b)

	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseFailed {
		t.Fatalf("phase %q", got.Status.Phase)
	}
	// The tenant has to be able to see which field of their spec was wrong.
	if !strings.Contains(got.Status.Reason, "filesystem.mounts[0].path") {
		t.Errorf("reason lost execd's explanation: %q", got.Status.Reason)
	}
}

func TestSandboxExpiresAtItsDeadline(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312", func(s *sbxv1.Sandbox) { s.Spec.TTLSeconds = 1 })
	c := newFakeClient(t, tpl, pod, sbx)
	b := &fakeBinder{}

	now := time.Now()
	r := newSandboxReconciler(t, c, b)
	r.Now = func() time.Time { return now }

	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %q", got.Status.Phase)
	}

	// Step past the deadline.
	r.Now = func() time.Time { return now.Add(time.Hour) }
	reconcileSandbox(t, r, "sbx-1")

	var after sbxv1.Sandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "sbx-1"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != sbxv1.PhaseExpired {
		t.Fatalf("phase %q, want Expired: a sandbox outliving its TTL is the platform's worst leak", after.Status.Phase)
	}
	if len(listPods(t, c)) != 0 {
		t.Error("expiry must delete the Pod, not just relabel the Sandbox")
	}
	if len(b.unbinds) == 0 {
		t.Error("execd should be asked to wind down so tenant processes get a SIGTERM first")
	}
}

func TestSandboxFinalizerRemovesThePod(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, pod, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	drive(t, r, c, "sbx-1", 6)

	var live sbxv1.Sandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "sbx-1"}, &live); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&live, sbxv1.FinalizerSandbox) {
		t.Fatal("the finalizer is what stops a deleted Sandbox from orphaning its Pod")
	}
	if err := c.Delete(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	reconcileSandbox(t, r, "sbx-1")

	if len(listPods(t, c)) != 0 {
		t.Error("deleting a Sandbox must take its Pod with it")
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "sbx-1"}, &live)
	if !apiNotFound(err) {
		t.Errorf("the Sandbox should be gone once the finalizer is cleared, got %v", err)
	}
}

func TestSandboxRejectsAnUnapprovedTemplate(t *testing.T) {
	tpl := approvedTemplate("py312")
	tpl.Status.Admission = sbxv1.AdmissionRejected
	tpl.Status.AdmissionReason = "image is not pinned by digest"
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	got := drive(t, r, c, "sbx-1", 4)
	if got.Status.Phase != sbxv1.PhaseFailed {
		t.Fatalf("phase %q", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Reason, "not pinned by digest") {
		t.Errorf("reason should repeat the admission verdict: %q", got.Status.Reason)
	}
}

func TestSandboxMissingTemplateFailsClearly(t *testing.T) {
	sbx := newSandbox("sbx-1", "nope")
	c := newFakeClient(t, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	got := drive(t, r, c, "sbx-1", 4)
	if got.Status.Phase != sbxv1.PhaseFailed || !strings.Contains(got.Status.Reason, `template "nope"`) {
		t.Fatalf("phase %q reason %q", got.Status.Phase, got.Status.Reason)
	}
}

func TestTTLIsClampedToTheTemplateCeiling(t *testing.T) {
	tpl := approvedTemplate("py312", func(x *sbxv1.SandboxTemplate) { x.Spec.MaxTTLSeconds = 60 })
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312", func(s *sbxv1.Sandbox) { s.Spec.TTLSeconds = 99999 })
	c := newFakeClient(t, tpl, pod, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	drive(t, r, c, "sbx-1", 6)
	if len(b.calls) != 1 || b.calls[0].TTLSeconds != 60 {
		t.Fatalf("TTL was not clamped to the template ceiling: %+v", b.calls)
	}
}

// step runs the reconciler n times, which is how the manager's requeue loop
// behaves: the first pass installs the finalizer and asks to be called again.
func step(t *testing.T, r *SandboxReconciler, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		reconcileSandbox(t, r, name)
	}
}

func apiNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

var _ = client.ObjectKey{}

// ── write conflicts ─────────────────────────────────────────────────────────

// Losing a write race is ordinary: this reconciler writes status using an
// object read from an informer cache that may not have caught up with its own
// previous write. It must retry quietly rather than surface an error, because a
// log full of routine contention is a log nobody reads when something is
// actually wrong.
func TestAStatusConflictIsRetriedNotReportedAsAnError(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")

	var conflicts atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(tpl, pod, sbx).
		WithStatusSubresource(&sbxv1.Sandbox{}, &sbxv1.SandboxTemplate{}, &sbxv1.SandboxPool{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, opts ...client.SubResourceUpdateOption) error {
				// Fail the first status write only, the way a stale cached read
				// would.
				if conflicts.Add(1) == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "sandbox.jdix.io", Resource: "sandboxes"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).Build()

	r := newSandboxReconciler(t, c, &fakeBinder{})
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: "sbx-1"},
	})
	if err != nil {
		t.Fatalf("a conflict must not surface as a reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatal("a conflict must be retried")
	}
	if res.RequeueAfter > time.Second {
		t.Errorf("the retry is on someone's request path; %v is too long", res.RequeueAfter)
	}

	// And the retry settles.
	got := drive(t, r, c, "sbx-1", 6)
	if got.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %q reason %q", got.Status.Phase, got.Status.Reason)
	}
}

// A genuine write failure must still be reported. Swallowing every error in the
// name of quieting conflicts would hide the ones worth waking up for.
func TestANonConflictWriteFailureIsStillAnError(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(tpl, pod, sbx).
		WithStatusSubresource(&sbxv1.Sandbox{}, &sbxv1.SandboxTemplate{}, &sbxv1.SandboxPool{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return apierrors.NewInternalError(errors.New("etcd is unhappy"))
			},
		}).Build()

	r := newSandboxReconciler(t, c, &fakeBinder{})
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: "sbx-1"},
	})
	if err == nil {
		t.Fatal("an internal error must not be mistaken for contention")
	}
}

// Adding the finalizer used to end the pass and requeue, which meant the next
// one re-read from a cache that had not caught up and then wrote status against
// a stale version — the conflict this whole path exists to avoid. One pass now
// does both.
func TestTheFinalizerAndTheFirstBindHappenInOnePass(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, pod, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	reconcileSandbox(t, r, "sbx-1")

	var got sbxv1.Sandbox
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: "sbx-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, sbxv1.FinalizerSandbox) {
		t.Fatal("the finalizer was not added")
	}
	if got.Status.PodName == "" {
		t.Fatal("the same pass should also have claimed a Pod")
	}
}

// The claim and the status write are two operations, and the second can fail.
// When it does, the Pod is already stamped with the sandbox id — so the next
// pass must adopt it rather than take a second one. A lost status write should
// cost a retry, not a Pod that nothing will ever collect.
func TestALostStatusWriteDoesNotStrandAPod(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")

	var writes atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(tpl, pod, sbx).
		WithStatusSubresource(&sbxv1.Sandbox{}, &sbxv1.SandboxTemplate{}, &sbxv1.SandboxPool{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if writes.Add(1) == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "sandbox.jdix.io", Resource: "sandboxes"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).Build()

	r := newSandboxReconciler(t, c, &fakeBinder{})
	got := drive(t, r, c, "sbx-1", 8)

	if got.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %q reason %q", got.Status.Phase, got.Status.Reason)
	}
	if got.Status.PodName != "warm-1" {
		t.Fatalf("bound to %q; the Pod claimed before the failed write should have been adopted",
			got.Status.PodName)
	}
	if n := len(listPods(t, c)); n != 1 {
		t.Fatalf("%d Pods exist; a lost status write must not cost a second one", n)
	}
}

// A Pod built by an older controller carries no control-plane credential.
// Binding it would fail with a bare 401 that says nothing about the real cause,
// so it is passed over and left for the pool's staleness sweep.
func TestAPodWithoutATokenIsNotClaimed(t *testing.T) {
	tpl := approvedTemplate("py312")
	stale := warmPod("stale-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	// Strip the credential, the way a Pod from a previous version would look.
	stale.Spec.Containers[0].Env = nil
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, stale, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	reconcileSandbox(t, r, "sbx-1")

	if p := mustGetPod(t, c, "stale-1"); p.Labels[sbxv1.LabelState] != sbxv1.StateIdle {
		t.Fatal("a Pod with no credential must not be claimed")
	}
	if len(b.calls) != 0 {
		t.Fatal("nothing should have been bound")
	}
	// A cold start covers the request instead of failing it.
	if len(listPods(t, c)) != 2 {
		t.Fatalf("expected a cold start alongside the unusable Pod, got %d Pods", len(listPods(t, c)))
	}
}
