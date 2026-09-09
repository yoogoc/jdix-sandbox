package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

// AnnotationApprovedBy records who signed off on a template that needed review.
const AnnotationApprovedBy = "sandbox.jdix.io/approved-by"

// TemplateReconciler publishes a template's hash and runs admission.
type TemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AllowedRegistries is the set of hosts a tenant image may come from.
	// Anonymous public registries are excluded on purpose: their rate limits
	// turn into cold starts that fail at random, which is very hard to diagnose
	// from the symptom.
	AllowedRegistries []string
	// Platform and Layout are needed because the template hash fingerprints the
	// Pod this controller would build, not just the template's own fields.
	Platform PlatformImage
	Layout   bwrap.Layout

	// MaxImageBytes bounds how large a tenant image may be. A very large image
	// turns every cold start into a minutes-long wait, which looks to the tenant
	// like the platform being broken.
	MaxImageBytes int64

	// Resolver turns a tag into a digest. Left nil, only references that already
	// carry a digest are accepted — which is the right behaviour for an
	// air-gapped install, and for local development against an image that was
	// never pushed anywhere.
	Resolver ImageResolver

	// Now is injectable so tests are not at the mercy of the clock.
	Now func() time.Time
}

// resolveRetry is how long to wait before trying a registry again. Registry
// outages are common and brief; permanently rejecting a template because of one
// would be wrong.
const resolveRetry = 30 * time.Second

// defaultMaxImageBytes is 5 GiB.
const defaultMaxImageBytes = 5 << 30

func (r *TemplateReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxtemplates/status,verbs=get;update;patch
// Reading imagePullSecrets is what lets a template resolve the same image its
// Pods will pull, rather than whatever an anonymous request happens to see.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *TemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tpl sbxv1.SandboxTemplate
	if err := r.Get(ctx, req.NamespacedName, &tpl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tpl.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Checks that need nothing but the spec come first, so an obviously wrong
	// template is answered immediately rather than after a registry round trip.
	if state, reason := r.staticAdmit(&tpl); state == sbxv1.AdmissionRejected {
		return ctrl.Result{}, r.publish(ctx, &tpl, state, reason)
	}

	requeue, err := r.resolveImage(ctx, &tpl)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue > 0 {
		// A transient registry failure: the template stays Pending and is
		// retried, rather than being rejected for something that is not its
		// fault.
		if err := r.publish(ctx, &tpl, sbxv1.AdmissionPending, tpl.Status.AdmissionReason); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if tpl.Status.Admission == sbxv1.AdmissionRejected {
		// resolveImage settled it: the image genuinely does not exist.
		return ctrl.Result{}, r.publish(ctx, &tpl, sbxv1.AdmissionRejected, tpl.Status.AdmissionReason)
	}

	state, reason := r.resolvedAdmit(&tpl)
	return ctrl.Result{}, r.publish(ctx, &tpl, state, reason)
}

// publish writes status only when something actually changed, so a template
// that has settled stops generating updates.
func (r *TemplateReconciler) publish(ctx context.Context, tpl *sbxv1.SandboxTemplate, state sbxv1.AdmissionState, reason string) error {
	hash := TemplateHash(tpl, r.Platform, r.Layout)
	if tpl.Status.Hash == hash &&
		tpl.Status.Admission == state &&
		tpl.Status.AdmissionReason == reason &&
		tpl.Status.HasVolumes == (len(tpl.Spec.Volumes) > 0) {
		return nil
	}
	tpl.Status.Hash = hash
	tpl.Status.Admission = state
	tpl.Status.AdmissionReason = reason
	tpl.Status.HasVolumes = len(tpl.Spec.Volumes) > 0
	if err := r.Status().Update(ctx, tpl); err != nil && !apierrors.IsConflict(err) {
		return err
	}
	// A conflict here is harmless: the next reconcile recomputes the same
	// verdict from the newer object.
	return nil
}

// resolveImage turns a tag into a digest, once.
//
// It returns a non-zero duration when the caller should try again later, and
// sets Admission to Rejected on the status when the image is genuinely absent.
func (r *TemplateReconciler) resolveImage(ctx context.Context, tpl *sbxv1.SandboxTemplate) (time.Duration, error) {
	ref := tpl.Spec.Image.Ref

	// An explicit digest is already immutable; there is nothing to resolve.
	if d := digestOf(ref); d != "" {
		tpl.Status.ResolvedDigest, tpl.Status.ResolvedRef = d, ref
		tpl.Status.ImagePinned = true
		return 0, nil
	}

	// The template opted out. The kubelet will pull by tag when a Pod starts,
	// which is what makes this work for an image the platform cannot see but the
	// nodes can. Nothing downstream is pinned, and status says so.
	if !tpl.Spec.Image.ShouldResolve() {
		tpl.Status.ResolvedDigest, tpl.Status.ResolvedRef = "", ""
		tpl.Status.ResolvedAt, tpl.Status.ImageBytes = nil, 0
		tpl.Status.ImagePinned = false
		return 0, nil
	}

	// Already resolved, and the reference has not changed since.
	if tpl.Status.ResolvedDigest != "" && tpl.Status.ResolvedRef == ref {
		tpl.Status.ImagePinned = true
		return 0, nil
	}
	if r.Resolver == nil {
		tpl.Status.Admission = sbxv1.AdmissionRejected
		tpl.Status.AdmissionReason = "this installation cannot resolve tags: either pin spec.image.ref by digest (name@sha256:...), or set spec.image.resolve=false to let the kubelet pull by tag"
		return 0, nil
	}

	var secrets []string
	if tpl.Spec.Image.PullSecretRef != nil {
		secrets = append(secrets, tpl.Spec.Image.PullSecretRef.Name)
	}

	res, err := r.Resolver.Resolve(ctx, ref, secrets, tpl.Namespace)
	if err != nil {
		if errors.Is(err, ErrImageNotFound) {
			tpl.Status.Admission = sbxv1.AdmissionRejected
			tpl.Status.AdmissionReason = fmt.Sprintf(
				"cannot pull %s: it does not exist, or the pull secret cannot see it", ref)
			return 0, nil
		}
		log.FromContext(ctx).Info("image resolution failed, will retry",
			"template", tpl.Name, "ref", ref, "err", err.Error())
		tpl.Status.AdmissionReason = "resolving " + ref + ": " + err.Error()
		return resolveRetry, nil
	}

	now := metav1.NewTime(r.now())
	tpl.Status.ResolvedDigest = res.Digest
	tpl.Status.ResolvedRef = ref
	tpl.Status.ResolvedAt = &now
	tpl.Status.ImageBytes = res.Bytes
	tpl.Status.ImagePinned = true
	tpl.Status.AdmissionReason = ""
	return 0, nil
}

// staticAdmit runs the checks that need only the spec.
//
// spec.image.ref accepts either form — name:tag or name@sha256:... A tag is
// what people actually write, and demanding a digest here would push work onto
// the tenant that they cannot even do until after a push. The tag is resolved
// once, in resolveImage, and pinned from then on.
func (r *TemplateReconciler) staticAdmit(tpl *sbxv1.SandboxTemplate) (sbxv1.AdmissionState, string) {
	ref := tpl.Spec.Image.Ref
	if ref == "" {
		return sbxv1.AdmissionRejected, "spec.image.ref is required"
	}
	if host := registryOf(ref); !r.registryAllowed(host) {
		return sbxv1.AdmissionRejected, fmt.Sprintf(
			"registry %q is not on the allowlist (%s)", host, strings.Join(r.AllowedRegistries, ", "))
	}
	// Nothing a tenant writes may land where the platform's own binaries go;
	// the initContainer would overwrite it, and finding that out at runtime is
	// far worse than being told at submission.
	if tpl.Spec.FilesystemDefaults.Workspace.Path != "" {
		p := tpl.Spec.FilesystemDefaults.Workspace.Path
		if p == "/opt/jdix" || strings.HasPrefix(p, "/opt/jdix/") ||
			p == "/var/lib/jdix" || strings.HasPrefix(p, "/var/lib/jdix/") {
			return sbxv1.AdmissionRejected, "workspace path overlaps the platform directory"
		}
	}

	return sbxv1.AdmissionApproved, ""
}

// resolvedAdmit runs the checks that need the image to have been resolved.
func (r *TemplateReconciler) resolvedAdmit(tpl *sbxv1.SandboxTemplate) (sbxv1.AdmissionState, string) {
	max := r.MaxImageBytes
	if max <= 0 {
		max = defaultMaxImageBytes
	}
	// Zero means the size is unknown, which is the case for a reference that
	// arrived already pinned. Rejecting on an unknown size would block exactly
	// the air-gapped installs that cannot resolve in the first place.
	if b := tpl.Status.ImageBytes; b > 0 && b > max {
		return sbxv1.AdmissionRejected, fmt.Sprintf(
			"image is %.1f GiB, over the %.1f GiB limit; a cold start would take minutes",
			float64(b)/(1<<30), float64(max)/(1<<30))
	}

	if len(tpl.Spec.Volumes) > 0 {
		// A volume implies a warm pool — a CSI attach is far too slow to sit on
		// the request path — and a warm pool is capacity the platform pays for.
		// So a human decides.
		if tpl.Annotations[AnnotationApprovedBy] == "" {
			return sbxv1.AdmissionPendingReview,
				"templates with volumes need review: a volume requires a warm pool, and warm capacity is charged to the platform"
		}
	}
	return sbxv1.AdmissionApproved, ""
}

func (r *TemplateReconciler) registryAllowed(host string) bool {
	if len(r.AllowedRegistries) == 0 {
		return true // no allowlist configured: nothing to enforce
	}
	for _, a := range r.AllowedRegistries {
		if host == a {
			return true
		}
	}
	return false
}

// digestOf returns the sha256 digest of an image reference, or "" if the
// reference is not pinned.
func digestOf(ref string) string {
	i := strings.Index(ref, "@sha256:")
	if i < 0 {
		return ""
	}
	d := ref[i+1:]
	if len(d) != len("sha256:")+64 {
		return ""
	}
	return d
}

// registryOf extracts the host from an image reference. A first component with
// no dot, colon or "localhost" is a Docker Hub namespace, not a host.
func registryOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	first, rest, ok := strings.Cut(ref, "/")
	if !ok {
		return "docker.io"
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		_ = rest
		return "docker.io"
	}
	return first
}

// SetupWithManager wires the template reconciler up.
func (r *TemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sbxv1.SandboxTemplate{}).
		Named("sandboxtemplate").
		Complete(r)
}
