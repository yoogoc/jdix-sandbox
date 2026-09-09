package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

func newPool(name, template string, replicas int32, mods ...func(*sbxv1.SandboxPool)) *sbxv1.SandboxPool {
	p := &sbxv1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: sbxv1.SandboxPoolSpec{
			TemplateRef:            template,
			Replicas:               replicas,
			MaxReplicas:            10,
			MaxSurge:               3,
			NotReadyTimeoutSeconds: 300,
			IdleTTLSeconds:         3600,
		},
	}
	for _, m := range mods {
		m(p)
	}
	return p
}

func newPoolReconciler(t *testing.T, c client.Client, b *fakeBinder) *PoolReconciler {
	t.Helper()
	return &PoolReconciler{
		Client:   c,
		Scheme:   testScheme(t),
		Layout:   bwrap.DefaultLayout(),
		Platform: PlatformImage{Ref: "registry.internal/jdix/platform@sha256:" + repeat64('b')},
		Prober:   b,
		NewToken: func() string { return "jct_pool_test" },
	}
}

func reconcilePool(t *testing.T, r *PoolReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile pool: %v", err)
	}
	return res
}

func poolPods(t *testing.T, c client.Client, pool string) []corev1.Pod {
	t.Helper()
	var l corev1.PodList
	if err := c.List(context.Background(), &l, client.InNamespace(testNS),
		client.MatchingLabels{sbxv1.LabelPool: pool}); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestPoolFillsToTheTarget(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 3)
	c := newFakeClient(t, tpl, pool)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "py312")
	if got := len(poolPods(t, c, "py312")); got != 3 {
		t.Fatalf("created %d warm Pods, want 3", got)
	}

	// Warm Pods are bare Pods, not a Deployment: bind works by taking one out of
	// the pool, and an owning ReplicaSet would fight that.
	for _, p := range poolPods(t, c, "py312") {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.Kind != "SandboxPool" {
			t.Fatalf("warm Pod owner is %+v, want the SandboxPool", owner)
		}
		if p.Labels[sbxv1.LabelState] != sbxv1.StateIdle {
			t.Fatalf("warm Pod state is %q", p.Labels[sbxv1.LabelState])
		}
	}
}

func TestPoolDoesNotOversupplyWhilePodsAreStarting(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 2)
	c := newFakeClient(t, tpl, pool)
	r := newPoolReconciler(t, c, &fakeBinder{})

	// Three passes with nothing becoming ready must still leave two Pods:
	// counting only ready Pods would spawn a new batch every round.
	for i := 0; i < 3; i++ {
		reconcilePool(t, r, "py312")
	}
	if got := len(poolPods(t, c, "py312")); got != 2 {
		t.Fatalf("pool has %d Pods, want 2; starting Pods must count towards the target", got)
	}
}

func TestPoolMeasuresReadyPodsAndRecordsTheTier(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 1)
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, "", true)
	pod.Labels[sbxv1.LabelPool] = "py312"
	c := newFakeClient(t, tpl, pool, pod)
	b := &fakeBinder{tier: bwrap.TierUserns}
	r := newPoolReconciler(t, c, b)

	reconcilePool(t, r, "py312")

	got := mustGetPod(t, c, "warm-1")
	if got.Labels[sbxv1.LabelIsolationTier] != string(bwrap.TierUserns) {
		t.Fatalf("tier label is %q; measuring during supply is what keeps it off the request path",
			got.Labels[sbxv1.LabelIsolationTier])
	}
	if got.Annotations[sbxv1.AnnotationTierProbe] == "" {
		t.Error("the probe's explanation should be recorded for the health page")
	}
}

func TestPoolDiscardsNodesBelowTheTemplateFloor(t *testing.T) {
	tpl := approvedTemplate("py312") // wants userns
	pool := newPool("py312", "py312", 1)
	pod := warmPod("warm-1", "py312", tpl.Status.Hash, "", true)
	pod.Labels[sbxv1.LabelPool] = "py312"
	c := newFakeClient(t, tpl, pool, pod)
	b := &fakeBinder{tier: bwrap.TierChroot}
	r := newPoolReconciler(t, c, b)

	reconcilePool(t, r, "py312")

	// The weak Pod is dropped on the supply path, so no request ever meets it.
	var got corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "warm-1"}, &got)
	if !apiNotFound(err) {
		t.Fatalf("a Pod that cannot meet the floor must be discarded during supply, got %v", err)
	}
}

func TestPoolRetiresStalePodsWithinMaxSurge(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 5, func(p *sbxv1.SandboxPool) { p.Spec.MaxSurge = 2 })
	objs := []client.Object{tpl, pool}
	for _, n := range []string{"old-1", "old-2", "old-3", "old-4"} {
		p := warmPod(n, "py312", "oldhash", string(bwrap.TierUserns), true)
		p.Labels[sbxv1.LabelPool] = "py312"
		objs = append(objs, p)
	}
	c := newFakeClient(t, objs...)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "py312")

	stale := 0
	for _, p := range poolPods(t, c, "py312") {
		if p.Labels[sbxv1.LabelTemplateHash] == "oldhash" {
			stale++
		}
	}
	if stale != 2 {
		t.Fatalf("%d stale Pods remain, want 2: a rollout must be bounded by maxSurge", stale)
	}
}

func TestPoolDeletesPodsStuckNotReady(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 1, func(p *sbxv1.SandboxPool) { p.Spec.NotReadyTimeoutSeconds = 60 })
	stuck := warmPod("stuck", "py312", tpl.Status.Hash, "", false)
	stuck.Labels[sbxv1.LabelPool] = "py312"
	stuck.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	c := newFakeClient(t, tpl, pool, stuck)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "py312")

	var got corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "stuck"}, &got)
	if !apiNotFound(err) {
		t.Fatalf("a Pod stuck past the timeout should be replaced, got %v", err)
	}
}

// The timeout that reaps stuck Pods is also the one that can destroy a pool: if
// it is shorter than a CSI attach, every Pod is deleted just before it would
// have become ready, and the pool rebuilds forever without ever filling.
func TestPoolRespectsALongNotReadyTimeoutForVolumeTemplates(t *testing.T) {
	tpl := approvedTemplate("with-vol", func(x *sbxv1.SandboxTemplate) {
		x.Spec.Volumes = []sbxv1.TemplateVolume{{Name: "corpus", ClaimName: "corpus-rox", ReadOnly: true}}
	})
	pool := newPool("with-vol", "with-vol", 1, func(p *sbxv1.SandboxPool) {
		p.Spec.NotReadyTimeoutSeconds = 600
	})
	attaching := warmPod("attaching", "with-vol", tpl.Status.Hash, "", false)
	attaching.Labels[sbxv1.LabelPool] = "with-vol"
	attaching.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Minute))
	c := newFakeClient(t, tpl, pool, attaching)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "with-vol")

	if _, err := getPod(c, "attaching"); err != nil {
		t.Fatal("a Pod still within its not-ready budget must be left alone; deleting it makes the pool never fill")
	}
	if got := len(poolPods(t, c, "with-vol")); got != 1 {
		t.Fatalf("pool has %d Pods, want 1", got)
	}
}

func TestPoolWillNotSupplyAnUnapprovedTemplate(t *testing.T) {
	tpl := approvedTemplate("with-vol")
	tpl.Status.Admission = sbxv1.AdmissionPendingReview
	pool := newPool("with-vol", "with-vol", 3)
	c := newFakeClient(t, tpl, pool)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "with-vol")

	if got := len(poolPods(t, c, "with-vol")); got != 0 {
		t.Fatalf("created %d Pods for a template awaiting review; warm capacity is spend that review authorises", got)
	}
}

func TestPoolCollectsOrphanedBoundPods(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 0)
	orphan := warmPod("orphan", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	orphan.Labels[sbxv1.LabelPool] = "py312"
	orphan.Labels[sbxv1.LabelState] = sbxv1.StateBound
	orphan.Labels[sbxv1.LabelSandboxID] = "sbx-gone"
	c := newFakeClient(t, tpl, pool, orphan)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "py312")

	if _, err := getPod(c, "orphan"); err == nil {
		t.Fatal("a bound Pod whose Sandbox no longer exists is a leak and must be collected")
	}
}

func TestPoolStatusReportsWhatOperatorsNeed(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 2)
	ready := warmPod("ready-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	ready.Labels[sbxv1.LabelPool] = "py312"
	bound := warmPod("bound-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	bound.Labels[sbxv1.LabelPool] = "py312"
	bound.Labels[sbxv1.LabelState] = sbxv1.StateBound
	bound.Labels[sbxv1.LabelSandboxID] = "sbx-1"
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, pool, ready, bound, sbx)
	r := newPoolReconciler(t, c, &fakeBinder{})

	reconcilePool(t, r, "py312")

	var got sbxv1.SandboxPool
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "py312"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Idle != 1 || got.Status.Bound != 1 {
		t.Fatalf("status idle=%d bound=%d, want 1/1", got.Status.Idle, got.Status.Bound)
	}
	if got.Status.IdleByTier[string(bwrap.TierUserns)] != 1 {
		t.Fatalf("idleByTier is %v; a Pod only counts for a template whose floor its node meets", got.Status.IdleByTier)
	}
	if got.Status.TemplateHash != tpl.Status.Hash {
		t.Errorf("status hash %q, want %q", got.Status.TemplateHash, tpl.Status.Hash)
	}
}

func TestDesiredReplicasIsClamped(t *testing.T) {
	cases := []struct {
		replicas, min, max int32
		want               int
	}{
		{3, 0, 10, 3},
		{0, 2, 10, 2},   // min raises it
		{50, 0, 10, 10}, // max is what makes a burst fall through to cold starts
		{-5, 0, 10, 0},
	}
	for _, tc := range cases {
		p := newPool("p", "t", tc.replicas, func(x *sbxv1.SandboxPool) {
			x.Spec.MinReplicas, x.Spec.MaxReplicas = tc.min, tc.max
		})
		if got := desiredReplicas(p); got != tc.want {
			t.Errorf("replicas=%d min=%d max=%d: got %d want %d", tc.replicas, tc.min, tc.max, got, tc.want)
		}
	}
}

func getPod(c client.Client, name string) (*corev1.Pod, error) {
	var p corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &p)
	return &p, err
}
