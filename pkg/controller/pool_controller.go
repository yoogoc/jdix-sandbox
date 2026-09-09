package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

// PoolReconciler keeps warm Pods available for a template.
//
// Warm Pods are created as bare Pods rather than through a Deployment. Binding
// works by taking a Pod out of the pool and re-parenting it, and a ReplicaSet
// would either recreate it immediately or, worse, decide the bound Pod is
// surplus and delete it out from under a tenant.
type PoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Layout   bwrap.Layout
	Platform PlatformImage
	Prober   Prober

	// NewToken mints each Pod's control-plane credential. Injectable so tests
	// are not at the mercy of randomness.
	NewToken func() string
	Now      func() time.Time
}

func (r *PoolReconciler) newToken() string {
	if r.NewToken != nil {
		return r.NewToken()
	}
	return NewControlToken()
}

// poolResync is the idle cadence. Supply is not latency-critical — a request
// that misses the pool cold-starts rather than waiting for it — so this is
// deliberately unhurried.
const poolResync = 10 * time.Second

func (r *PoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxpools/status,verbs=get;update;patch

func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var pool sbxv1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var tpl sbxv1.SandboxTemplate
	tplKey := client.ObjectKey{Namespace: pool.Namespace, Name: pool.Spec.TemplateRef}
	if err := r.Get(ctx, tplKey, &tpl); err != nil {
		if apierrors.IsNotFound(err) {
			lg.Info("pool references a template that does not exist", "template", pool.Spec.TemplateRef)
			return ctrl.Result{RequeueAfter: poolResync}, nil
		}
		return ctrl.Result{}, err
	}
	// A template still awaiting review must not consume warm capacity: warm
	// Pods cost the platform money, and review is what authorises that spend.
	if tpl.Status.Admission != sbxv1.AdmissionApproved {
		return r.publishStatus(ctx, &pool, poolState{}, TemplateHash(&tpl, r.Platform, r.Layout), poolResync)
	}

	hash := TemplateHash(&tpl, r.Platform, r.Layout)
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{sbxv1.LabelPool: pool.Name}); err != nil {
		return ctrl.Result{}, err
	}

	st := r.classify(ctx, &pool, &tpl, hash, pods.Items)

	// Replace stale Pods before adding new ones, so a rollout converges instead
	// of doubling the pool first.
	surge := int(pool.Spec.MaxSurge)
	if surge <= 0 {
		surge = 3
	}
	for i, p := range st.stale {
		if i >= surge {
			break
		}
		lg.Info("retiring stale warm Pod", "pod", p.Name, "wantHash", hash)
		_ = r.delete(ctx, p)
	}
	for _, p := range st.expired {
		_ = r.delete(ctx, p)
	}
	for _, p := range st.stuck {
		lg.Info("deleting stuck warm Pod", "pod", p.Name, "age", r.now().Sub(p.CreationTimestamp.Time).String())
		_ = r.delete(ctx, p)
	}
	for _, p := range st.orphans {
		lg.Info("deleting orphaned bound Pod", "pod", p.Name)
		_ = r.delete(ctx, p)
	}

	desired := desiredReplicas(&pool)
	have := len(st.idleCurrent) + len(st.pending)
	for i := have; i < desired; i++ {
		pod := BuildPod(&tpl, pool.Name, r.Platform, r.Layout, r.newToken())
		if err := controllerutil.SetControllerReference(&pool, pod, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, pod); err != nil {
			lg.Error(err, "creating warm Pod")
			break
		}
	}
	// Trim, oldest first: the oldest warm Pod has the stalest page cache and the
	// most drift from the current image layers.
	for i := 0; i < len(st.idleCurrent)-desired; i++ {
		_ = r.delete(ctx, st.idleCurrent[i])
	}

	return r.publishStatus(ctx, &pool, st, hash, poolResync)
}

type poolState struct {
	idleCurrent []*corev1.Pod // ready, current hash, measured
	pending     []*corev1.Pod // not ready yet, still within its grace period
	stale       []*corev1.Pod
	stuck       []*corev1.Pod
	expired     []*corev1.Pod
	orphans     []*corev1.Pod
	bound       int
	byTier      map[string]int32
}

// classify sorts the pool's Pods and measures any that are ready but unmeasured.
//
// Measuring here, on the supply path, is what keeps it off the request path: by
// the time a Sandbox looks for a Pod, every candidate already carries a tier
// label, so a node that cannot meet the template's floor never even appears.
func (r *PoolReconciler) classify(ctx context.Context, pool *sbxv1.SandboxPool, tpl *sbxv1.SandboxTemplate, hash string, pods []corev1.Pod) poolState {
	lg := log.FromContext(ctx)
	st := poolState{byTier: map[string]int32{}}

	notReadyTimeout := time.Duration(pool.Spec.NotReadyTimeoutSeconds) * time.Second
	if notReadyTimeout <= 0 {
		notReadyTimeout = 5 * time.Minute
	}
	idleTTL := time.Duration(pool.Spec.IdleTTLSeconds) * time.Second

	for i := range pods {
		p := &pods[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		switch p.Labels[sbxv1.LabelState] {
		case sbxv1.StateBound:
			st.bound++
			// A bound Pod whose Sandbox is gone is a leak. The Sandbox's
			// finalizer normally prevents this; this catches the cases it
			// cannot, such as a force-deleted object.
			if r.sandboxMissing(ctx, p) {
				st.orphans = append(st.orphans, p)
			}
			continue
		case sbxv1.StateDraining:
			continue
		}

		age := r.now().Sub(p.CreationTimestamp.Time)
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			st.stuck = append(st.stuck, p)
			continue
		}
		if !podReady(p) {
			if age > notReadyTimeout {
				st.stuck = append(st.stuck, p)
			} else {
				st.pending = append(st.pending, p)
			}
			continue
		}
		if p.Labels[sbxv1.LabelTemplateHash] != hash {
			st.stale = append(st.stale, p)
			continue
		}
		if idleTTL > 0 && age > idleTTL {
			st.expired = append(st.expired, p)
			continue
		}

		tier := bwrap.Tier(p.Labels[sbxv1.LabelIsolationTier])
		if tier == "" {
			measured, reason, err := r.measure(ctx, p)
			if err != nil {
				st.pending = append(st.pending, p)
				continue
			}
			tier = measured
			if !tier.AtLeast(bwrap.Tier(tpl.Spec.MinIsolationTier)) {
				// This is the point of measuring during supply: the Pod is
				// discarded now, quietly, instead of failing someone's request.
				lg.Info("node cannot meet the template's isolation floor",
					"pod", p.Name, "node", p.Spec.NodeName,
					"measured", tier, "required", tpl.Spec.MinIsolationTier, "reason", reason)
				st.stuck = append(st.stuck, p)
				continue
			}
		}
		st.byTier[string(tier)]++
		st.idleCurrent = append(st.idleCurrent, p)
	}
	return st
}

// measure probes a warm Pod and records the result on it, so the measurement is
// made once per Pod rather than once per request.
func (r *PoolReconciler) measure(ctx context.Context, p *corev1.Pod) (bwrap.Tier, string, error) {
	if p.Status.PodIP == "" || r.Prober == nil {
		return "", "", errNoProbe
	}
	tier, reason, err := r.Prober.Probe(ctx, podRef(p))
	if err != nil {
		return "", "", err
	}
	patched := p.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = map[string]string{}
	}
	patched.Labels[sbxv1.LabelIsolationTier] = string(tier)
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[sbxv1.AnnotationTierProbe] = reason
	if err := r.Update(ctx, patched); err == nil {
		*p = *patched
	}
	return tier, reason, nil
}

func (r *PoolReconciler) sandboxMissing(ctx context.Context, p *corev1.Pod) bool {
	id := p.Labels[sbxv1.LabelSandboxID]
	if id == "" {
		return false
	}
	var sbx sbxv1.Sandbox
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: id}, &sbx)
	return apierrors.IsNotFound(err)
}

func (r *PoolReconciler) delete(ctx context.Context, p *corev1.Pod) error {
	return client.IgnoreNotFound(r.Delete(ctx, p))
}

func (r *PoolReconciler) publishStatus(ctx context.Context, pool *sbxv1.SandboxPool, st poolState, hash string, requeue time.Duration) (ctrl.Result, error) {
	pool.Status.Idle = int32(len(st.idleCurrent))
	pool.Status.Bound = int32(st.bound)
	pool.Status.Stale = int32(len(st.stale))
	pool.Status.NotReady = int32(len(st.pending))
	pool.Status.IdleByTier = st.byTier
	pool.Status.TemplateHash = hash
	if err := r.Status().Update(ctx, pool); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// desiredReplicas clamps the target between the configured bounds.
//
// MaxReplicas is not optional in practice: sizing a pool from peak demand at a
// short target horizon produces numbers far larger than the cluster, so the
// ceiling is what makes the burst fall through to cold starts instead.
func desiredReplicas(pool *sbxv1.SandboxPool) int {
	want := int(pool.Spec.Replicas)
	if min := int(pool.Spec.MinReplicas); want < min {
		want = min
	}
	if max := int(pool.Spec.MaxReplicas); max > 0 && want > max {
		want = max
	}
	if want < 0 {
		want = 0
	}
	return want
}

// SetupWithManager wires the pool reconciler up.
func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sbxv1.SandboxPool{}).
		Owns(&corev1.Pod{}).
		Named("sandboxpool").
		Complete(r)
}

type probeUnavailable struct{}

func (probeUnavailable) Error() string { return "no Pod IP or no prober configured" }

var errNoProbe error = probeUnavailable{}
