package controller

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"jdix.io/sandbox/pkg/api"
	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

// SandboxReconciler drives a Sandbox from Pending to Running and tears it down
// when its TTL elapses.
//
// Binding lives here rather than in the API server on purpose. Two components
// writing a Pod's state label would eventually let two Sandboxes believe they
// own the same Pod; one writer is worth more than the milliseconds a shortcut
// would save.
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Layout   bwrap.Layout
	Platform PlatformImage
	Binder   Binder
	Prober   Prober

	// TokenSecret holds the control-plane bearer token mounted into every Pod.
	TokenSecret string
	// EndpointFor renders the public URL for a sandbox.
	EndpointFor func(sandboxID string) string
	// Now and NewToken are injectable so tests are not at the mercy of the clock
	// or of randomness.
	Now      func() time.Time
	NewToken func() string
}

// requeueBinding is how often we re-check a Pod that is still starting. Short,
// because on a pool hit this is the only thing between a request and a ready
// sandbox.
const requeueBinding = 150 * time.Millisecond

func (r *SandboxReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *SandboxReconciler) newToken() string {
	if r.NewToken != nil {
		return r.NewToken()
	}
	var b [32]byte
	_, _ = crand.Read(b[:])
	return "sbt_" + hex.EncodeToString(b[:])
}

// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var sbx sbxv1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sbx); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sbx.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &sbx)
	}
	// The finalizer is what guarantees the Pod goes when the Sandbox does.
	// Without it a deleted Sandbox can leave a Pod running with nobody watching
	// it — the leak that fills a cluster fastest.
	if !controllerutil.ContainsFinalizer(&sbx, sbxv1.FinalizerSandbox) {
		controllerutil.AddFinalizer(&sbx, sbxv1.FinalizerSandbox)
		if err := r.Update(ctx, &sbx); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch sbx.Status.Phase {
	case "", sbxv1.PhasePending:
		return r.acquirePod(ctx, &sbx)
	case sbxv1.PhaseBinding:
		return r.bind(ctx, &sbx)
	case sbxv1.PhaseRunning:
		return r.watchExpiry(ctx, &sbx)
	case sbxv1.PhaseExpired, sbxv1.PhaseFailed:
		// Terminal. The Pod is already gone; the object stays until the tenant
		// or a retention sweep deletes it, so the outcome remains readable.
		return ctrl.Result{}, nil
	default:
		lg.Info("unknown phase, treating as pending", "phase", sbx.Status.Phase)
		return r.acquirePod(ctx, &sbx)
	}
}

// acquirePod claims a warm Pod, or creates one when the pool is empty.
func (r *SandboxReconciler) acquirePod(ctx context.Context, sbx *sbxv1.Sandbox) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	tpl, err := r.template(ctx, sbx)
	if err != nil {
		return r.fail(ctx, sbx, "TemplateUnavailable", err.Error())
	}
	if tpl.Status.Admission == sbxv1.AdmissionRejected {
		return r.fail(ctx, sbx, "TemplateRejected", "template failed admission: "+tpl.Status.AdmissionReason)
	}

	pod, err := r.claimWarmPod(ctx, sbx, tpl)
	if err != nil {
		return ctrl.Result{}, err
	}
	cold := pod == nil
	if cold {
		// The cold path is not a fallback to be tolerated but the entire
		// service whenever a pool is empty, so it gets the same treatment.
		pod, err = r.createPod(ctx, sbx, tpl)
		if err != nil {
			return r.fail(ctx, sbx, "PodCreateFailed", err.Error())
		}
		lg.Info("cold start", "sandbox", sbx.Name, "pod", pod.Name)
	}

	sbx.Status.Phase = sbxv1.PhaseBinding
	sbx.Status.PodName = pod.Name
	sbx.Status.ColdStart = cold
	if err := r.Status().Update(ctx, sbx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueBinding}, nil
}

// claimWarmPod flips one idle Pod to bound using an optimistic-concurrency
// update. A conflict means another reconcile took it, so we simply try the next
// candidate — no lease, no external lock, no single point of failure.
func (r *SandboxReconciler) claimWarmPod(ctx context.Context, sbx *sbxv1.Sandbox, tpl *sbxv1.SandboxTemplate) (*corev1.Pod, error) {
	var pods corev1.PodList
	err := r.List(ctx, &pods,
		client.InNamespace(sbx.Namespace),
		client.MatchingLabels{
			sbxv1.LabelTemplate:     tpl.Name,
			sbxv1.LabelTemplateHash: TemplateHash(tpl),
			sbxv1.LabelState:        sbxv1.StateIdle,
		})
	if err != nil {
		return nil, err
	}

	candidates := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if !podReady(p) || !p.DeletionTimestamp.IsZero() {
			continue
		}
		// Only Pods already measured at or above the template's floor. An
		// unmeasured Pod is skipped rather than probed here: probing on the
		// request path is exactly the latency the warm pool exists to avoid.
		tier := bwrap.Tier(p.Labels[sbxv1.LabelIsolationTier])
		if tier == "" || !tier.AtLeast(bwrap.Tier(tpl.Spec.MinIsolationTier)) {
			continue
		}
		candidates = append(candidates, p)
	}
	// Shuffle so concurrent reconciles do not all reach for the same Pod and
	// spend their time losing the same conflict.
	mrand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })

	for _, pod := range candidates {
		claimed := pod.DeepCopy()
		claimed.Labels[sbxv1.LabelState] = sbxv1.StateBound
		claimed.Labels[sbxv1.LabelSandboxID] = sbx.Name
		if tenant := sbx.Labels[sbxv1.LabelTenant]; tenant != "" {
			claimed.Labels[sbxv1.LabelTenant] = tenant
		}
		// Hand ownership from the pool to the Sandbox. From here the Pod's life
		// is the Sandbox's life, and the pool controller stops counting it.
		claimed.OwnerReferences = nil
		if err := controllerutil.SetControllerReference(sbx, claimed, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Update(ctx, claimed); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue // someone else got there first
			}
			return nil, err
		}
		return claimed, nil
	}
	return nil, nil
}

func (r *SandboxReconciler) createPod(ctx context.Context, sbx *sbxv1.Sandbox, tpl *sbxv1.SandboxTemplate) (*corev1.Pod, error) {
	pod := BuildPod(tpl, "", r.Platform, r.Layout, r.TokenSecret)
	pod.Labels[sbxv1.LabelState] = sbxv1.StateBound
	pod.Labels[sbxv1.LabelSandboxID] = sbx.Name
	if tenant := sbx.Labels[sbxv1.LabelTenant]; tenant != "" {
		pod.Labels[sbxv1.LabelTenant] = tenant
	}
	if err := controllerutil.SetControllerReference(sbx, pod, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// bind waits for the Pod to be ready, checks the node really can enforce what
// the template demands, and then hands execd the filesystem spec.
func (r *SandboxReconciler) bind(ctx context.Context, sbx *sbxv1.Sandbox) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: sbx.Namespace, Name: sbx.Status.PodName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, sbx, "PodLost", "the Pod backing this sandbox disappeared before it was bound")
		}
		return ctrl.Result{}, err
	}
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		return r.fail(ctx, sbx, "PodTerminated", "the Pod terminated before the sandbox was bound: "+pod.Status.Reason)
	}
	if !podReady(&pod) || pod.Status.PodIP == "" {
		return ctrl.Result{RequeueAfter: requeueBinding}, nil
	}

	tpl, err := r.template(ctx, sbx)
	if err != nil {
		return r.fail(ctx, sbx, "TemplateUnavailable", err.Error())
	}

	tier := bwrap.Tier(pod.Labels[sbxv1.LabelIsolationTier])
	if tier == "" {
		// A cold-started Pod has never been measured. Do it now, before it can
		// run anything.
		tier, _, err = r.Prober.Probe(ctx, pod.Status.PodIP)
		if err != nil {
			return ctrl.Result{RequeueAfter: requeueBinding}, nil
		}
	}
	if !tier.AtLeast(bwrap.Tier(tpl.Spec.MinIsolationTier)) {
		// Refuse rather than run with weaker isolation than promised. A silent
		// downgrade of a security guarantee is worse than a failed request.
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sbx, "IsolationUnavailable", fmt.Sprintf(
			"node %s can only provide %q isolation but the template requires %q",
			pod.Spec.NodeName, tier, tpl.Spec.MinIsolationTier))
	}

	token := r.newToken()
	ttl := ttlFor(sbx, tpl)
	resp, err := r.Binder.Bind(ctx, pod.Status.PodIP, api.BindRequest{
		SandboxID:  sbx.Name,
		Tenant:     sbx.Labels[sbxv1.LabelTenant],
		Filesystem: filesystemFor(sbx, tpl),
		Env:        envFor(sbx),
		TTLSeconds: int(ttl.Seconds()),
		Token:      token,
	})
	if err != nil {
		// A rejected spec is the tenant's problem to fix and will not get better
		// on retry, so it is terminal rather than a requeue loop.
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sbx, "BindFailed", err.Error())
	}

	boundAt := metav1.NewTime(r.now())
	expiresAt := metav1.NewTime(r.now().Add(ttl))
	sbx.Status.Phase = sbxv1.PhaseRunning
	sbx.Status.PodIP = pod.Status.PodIP
	sbx.Status.NodeName = pod.Spec.NodeName
	sbx.Status.IsolationTier = sbxv1.IsolationTier(resp.IsolationTier)
	sbx.Status.BoundAt = &boundAt
	sbx.Status.ExpiresAt = &expiresAt
	if r.EndpointFor != nil {
		sbx.Status.Endpoint = r.EndpointFor(sbx.Name)
	}
	metaHelper.setReady(&sbx.Status.Conditions, r.now())
	if err := r.Status().Update(ctx, sbx); err != nil {
		return ctrl.Result{}, err
	}

	// Record the tier on the Pod too, so a human reading `kubectl get pods` sees
	// what actually happened without cross-referencing.
	if pod.Labels[sbxv1.LabelIsolationTier] == "" {
		patched := pod.DeepCopy()
		patched.Labels[sbxv1.LabelIsolationTier] = string(tier)
		_ = r.Update(ctx, patched)
	}
	return ctrl.Result{RequeueAfter: expiresAt.Time.Sub(r.now())}, nil
}

// watchExpiry enforces the TTL from the controller's side.
//
// execd also terminates itself at the deadline. Both exist because they fail
// differently: execd dies with its Pod, and the controller keeps working when a
// Pod wedges. Either alone leaves a way for a sandbox to outlive its TTL.
func (r *SandboxReconciler) watchExpiry(ctx context.Context, sbx *sbxv1.Sandbox) (ctrl.Result, error) {
	if sbx.Status.ExpiresAt == nil {
		return ctrl.Result{}, nil
	}
	remaining := sbx.Status.ExpiresAt.Time.Sub(r.now())
	if remaining > 0 {
		// A little slack, so we act just after the deadline rather than racing it.
		return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
	}
	return r.expire(ctx, sbx, "TTL elapsed")
}

func (r *SandboxReconciler) expire(ctx context.Context, sbx *sbxv1.Sandbox, reason string) (ctrl.Result, error) {
	if err := r.deletePod(ctx, sbx); err != nil {
		return ctrl.Result{}, err
	}
	sbx.Status.Phase = sbxv1.PhaseExpired
	sbx.Status.Reason = reason
	return ctrl.Result{}, r.Status().Update(ctx, sbx)
}

func (r *SandboxReconciler) fail(ctx context.Context, sbx *sbxv1.Sandbox, reason, msg string) (ctrl.Result, error) {
	log.FromContext(ctx).Info("sandbox failed", "sandbox", sbx.Name, "reason", reason, "message", msg)
	sbx.Status.Phase = sbxv1.PhaseFailed
	sbx.Status.Reason = reason + ": " + msg
	return ctrl.Result{}, r.Status().Update(ctx, sbx)
}

func (r *SandboxReconciler) finalize(ctx context.Context, sbx *sbxv1.Sandbox) (ctrl.Result, error) {
	if err := r.deletePod(ctx, sbx); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(sbx, sbxv1.FinalizerSandbox)
	return ctrl.Result{}, r.Update(ctx, sbx)
}

func (r *SandboxReconciler) deletePod(ctx context.Context, sbx *sbxv1.Sandbox) error {
	if sbx.Status.PodName == "" {
		return nil
	}
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: sbx.Namespace, Name: sbx.Status.PodName}, &pod)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Ask execd to wind down first so the tenant's processes get a SIGTERM and a
	// grace period. Best effort: if the Pod is already unreachable, deleting it
	// is still the right next step.
	if pod.Status.PodIP != "" && r.Binder != nil {
		_ = r.Binder.Unbind(ctx, pod.Status.PodIP)
	}
	return client.IgnoreNotFound(r.Delete(ctx, &pod))
}

func (r *SandboxReconciler) template(ctx context.Context, sbx *sbxv1.Sandbox) (*sbxv1.SandboxTemplate, error) {
	var tpl sbxv1.SandboxTemplate
	key := client.ObjectKey{Namespace: sbx.Namespace, Name: sbx.Spec.TemplateRef}
	if err := r.Get(ctx, key, &tpl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("template %q does not exist in namespace %s", sbx.Spec.TemplateRef, sbx.Namespace)
		}
		return nil, err
	}
	return &tpl, nil
}

// ttlFor clamps the requested lifetime to the template's ceiling.
func ttlFor(sbx *sbxv1.Sandbox, tpl *sbxv1.SandboxTemplate) time.Duration {
	want := sbx.Spec.TTLSeconds
	if want <= 0 {
		want = tpl.Spec.DefaultTTLSeconds
	}
	if want <= 0 {
		want = 1800
	}
	if max := tpl.Spec.MaxTTLSeconds; max > 0 && want > max {
		want = max
	}
	return time.Duration(want) * time.Second
}

// filesystemFor merges the template's defaults under the sandbox's own spec.
func filesystemFor(sbx *sbxv1.Sandbox, tpl *sbxv1.SandboxTemplate) api.FilesystemSpec {
	out := api.FilesystemSpec{
		Workspace: api.Workspace{
			Path:      sbx.Spec.Filesystem.Workspace.Path,
			SizeLimit: sbx.Spec.Filesystem.Workspace.SizeLimit,
		},
		AllowSystemPaths: sbx.Spec.Filesystem.AllowSystemPaths,
		Hide:             sbx.Spec.Filesystem.Hide,
	}
	if out.Workspace.Path == "" {
		out.Workspace.Path = tpl.Spec.FilesystemDefaults.Workspace.Path
	}
	if out.Workspace.Path == "" {
		out.Workspace.Path = api.DefaultWorkspacePath
	}
	if len(out.AllowSystemPaths) == 0 {
		out.AllowSystemPaths = tpl.Spec.FilesystemDefaults.AllowSystemPaths
	}
	for _, m := range sbx.Spec.Filesystem.Mounts {
		out.Mounts = append(out.Mounts, api.Mount{
			Path:     m.Path,
			Source:   api.MountSource{Volume: m.Source.Volume, SubPath: m.Source.SubPath},
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func envFor(sbx *sbxv1.Sandbox) map[string]string {
	if len(sbx.Spec.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(sbx.Spec.Env))
	for _, e := range sbx.Spec.Env {
		out[e.Name] = e.Value
	}
	return out
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager wires the reconciler up. Pods are watched because a Pod
// becoming ready is what unblocks a pending bind.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sbxv1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Named("sandbox").
		Complete(r)
}
