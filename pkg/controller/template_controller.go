package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
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
	// MaxImageBytes is advisory here; the real check needs a registry client.
	MaxImageBytes int64
}

// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.jdix.io,resources=sandboxtemplates/status,verbs=get;update;patch

func (r *TemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tpl sbxv1.SandboxTemplate
	if err := r.Get(ctx, req.NamespacedName, &tpl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tpl.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	state, reason := r.admit(&tpl)

	hash := TemplateHash(&tpl)
	if tpl.Status.Hash == hash &&
		tpl.Status.Admission == state &&
		tpl.Status.AdmissionReason == reason {
		return ctrl.Result{}, nil
	}

	tpl.Status.Hash = hash
	tpl.Status.Admission = state
	tpl.Status.AdmissionReason = reason
	tpl.Status.HasVolumes = len(tpl.Spec.Volumes) > 0
	tpl.Status.ResolvedDigest = digestOf(tpl.Spec.Image.Ref)
	return ctrl.Result{}, r.Status().Update(ctx, &tpl)
}

// admit decides whether a template may be used, and says why when it may not.
func (r *TemplateReconciler) admit(tpl *sbxv1.SandboxTemplate) (sbxv1.AdmissionState, string) {
	ref := tpl.Spec.Image.Ref
	if ref == "" {
		return sbxv1.AdmissionRejected, "spec.image.ref is required"
	}
	// A tag can be repointed after admission, which would leave one warm pool
	// running two different builds of the same "version". Pinning by digest is
	// what makes the pool's contents knowable.
	if digestOf(ref) == "" {
		return sbxv1.AdmissionRejected,
			"spec.image.ref must be pinned by digest (name@sha256:...), not by tag"
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
