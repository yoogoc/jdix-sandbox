package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

func reconcileTemplate(t *testing.T, c client.Client, tpl *sbxv1.SandboxTemplate, registries ...string) *sbxv1.SandboxTemplate {
	t.Helper()
	r := &TemplateReconciler{Client: c, Scheme: testScheme(t), AllowedRegistries: registries}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: tpl.Namespace, Name: tpl.Name},
	}); err != nil {
		t.Fatalf("reconcile template: %v", err)
	}
	var got sbxv1.SandboxTemplate
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tpl.Namespace, Name: tpl.Name}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func rawTemplate(name, imageRef string, mods ...func(*sbxv1.SandboxTemplate)) *sbxv1.SandboxTemplate {
	t := &sbxv1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: sbxv1.SandboxTemplateSpec{
			MinIsolationTier: sbxv1.TierUserns,
			Image:            sbxv1.ImageSpec{Ref: imageRef},
		},
	}
	for _, m := range mods {
		m(t)
	}
	return t
}

func TestAdmissionRequiresADigest(t *testing.T) {
	// A tag can be repointed after admission, which would leave one warm pool
	// serving two different builds of the same "version".
	tpl := rawTemplate("py312", "registry.internal/jdix/py312:2026-09-01")
	got := reconcileTemplate(t, newFakeClient(t, tpl), tpl)
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q, want Rejected", got.Status.Admission)
	}
	if !strings.Contains(got.Status.AdmissionReason, "digest") {
		t.Errorf("reason should say what to fix: %q", got.Status.AdmissionReason)
	}
}

func TestAdmissionEnforcesTheRegistryAllowlist(t *testing.T) {
	tpl := rawTemplate("py312", "docker.io/library/python@sha256:"+repeat64('a'))
	got := reconcileTemplate(t, newFakeClient(t, tpl), tpl, "registry.internal")
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q, want Rejected", got.Status.Admission)
	}
	if !strings.Contains(got.Status.AdmissionReason, "docker.io") {
		t.Errorf("reason should name the offending registry: %q", got.Status.AdmissionReason)
	}
}

func TestAdmissionApprovesAPinnedImageFromAnAllowedRegistry(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/jdix/py312@sha256:"+repeat64('a'))
	got := reconcileTemplate(t, newFakeClient(t, tpl), tpl, "registry.internal")
	if got.Status.Admission != sbxv1.AdmissionApproved {
		t.Fatalf("admission %q reason %q", got.Status.Admission, got.Status.AdmissionReason)
	}
	if got.Status.Hash == "" || got.Status.ResolvedDigest == "" {
		t.Errorf("status is incomplete: %+v", got.Status)
	}
}

// A volume implies a warm pool, and warm capacity is spend the platform carries,
// so a template that declares one waits for a human.
func TestTemplatesWithVolumesWaitForReview(t *testing.T) {
	tpl := rawTemplate("with-vol", "registry.internal/x@sha256:"+repeat64('a'), func(x *sbxv1.SandboxTemplate) {
		x.Spec.Volumes = []sbxv1.TemplateVolume{{Name: "corpus", ClaimName: "corpus-rox"}}
	})
	c := newFakeClient(t, tpl)
	got := reconcileTemplate(t, c, tpl, "registry.internal")
	if got.Status.Admission != sbxv1.AdmissionPendingReview {
		t.Fatalf("admission %q, want PendingReview", got.Status.Admission)
	}
	if !got.Status.HasVolumes {
		t.Error("HasVolumes should be surfaced; it is what the review queue filters on")
	}

	// Once an administrator signs off, it goes through.
	got.Annotations = map[string]string{AnnotationApprovedBy: "ops@example.com"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	after := reconcileTemplate(t, c, got, "registry.internal")
	if after.Status.Admission != sbxv1.AdmissionApproved {
		t.Fatalf("admission %q after approval, reason %q", after.Status.Admission, after.Status.AdmissionReason)
	}
}

func TestAdmissionRejectsAWorkspaceOverThePlatformDirectory(t *testing.T) {
	tpl := rawTemplate("bad", "registry.internal/x@sha256:"+repeat64('a'), func(x *sbxv1.SandboxTemplate) {
		x.Spec.FilesystemDefaults.Workspace.Path = "/opt/jdix/work"
	})
	got := reconcileTemplate(t, newFakeClient(t, tpl), tpl, "registry.internal")
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q, want Rejected", got.Status.Admission)
	}
}

func TestRegistryOf(t *testing.T) {
	cases := map[string]string{
		"registry.internal/jdix/py312@sha256:x": "registry.internal",
		"localhost:5000/x@sha256:y":             "localhost:5000",
		"library/python@sha256:z":               "docker.io",
		"python@sha256:z":                       "docker.io",
		"ghcr.io/org/img@sha256:w":              "ghcr.io",
	}
	for ref, want := range cases {
		if got := registryOf(ref); got != want {
			t.Errorf("registryOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestTemplateHashTracksOnlyPodAffectingFields(t *testing.T) {
	base := approvedTemplate("py312")
	h := TemplateHash(base)

	// Metadata churn must not roll the pool.
	relabelled := base.DeepCopy()
	relabelled.Labels = map[string]string{"team": "search"}
	relabelled.Annotations = map[string]string{"note": "for the eval run"}
	if TemplateHash(relabelled) != h {
		t.Error("editing labels or annotations must not invalidate every warm Pod")
	}

	// Anything that changes the Pod must.
	for name, mod := range map[string]func(*sbxv1.SandboxTemplate){
		"image":   func(x *sbxv1.SandboxTemplate) { x.Spec.Image.Ref = "registry.internal/other@sha256:" + repeat64('c') },
		"tier":    func(x *sbxv1.SandboxTemplate) { x.Spec.MinIsolationTier = sbxv1.TierCapAdmin },
		"volumes": func(x *sbxv1.SandboxTemplate) { x.Spec.Volumes = []sbxv1.TemplateVolume{{Name: "v", ClaimName: "c"}} },
		"resources": func(x *sbxv1.SandboxTemplate) {
			x.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
		},
	} {
		changed := base.DeepCopy()
		mod(changed)
		if TemplateHash(changed) == h {
			t.Errorf("changing %s left the hash unchanged; stale Pods would never be replaced", name)
		}
	}
}

func TestBuildPodKeepsTheSecurityFloorEvenWithOverrides(t *testing.T) {
	tpl := approvedTemplate("py312", func(x *sbxv1.SandboxTemplate) {
		// An override that tries to take the boundary apart.
		x.Spec.PodOverrides = &corev1.PodSpec{
			HostNetwork:                  true,
			HostPID:                      true,
			AutomountServiceAccountToken: ptr(true),
			SecurityContext:              &corev1.PodSecurityContext{RunAsUser: ptr(int64(0))},
			NodeSelector:                 map[string]string{"pool": "sandbox"},
		}
	})
	pod := BuildPod(tpl, "py312", PlatformImage{Ref: "platform@sha256:" + repeat64('b')}, bwrap.DefaultLayout(), "tok")

	if pod.Spec.HostNetwork || pod.Spec.HostPID {
		t.Error("overrides must not be able to turn on host namespaces")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the ServiceAccount token must never be mounted into a sandbox Pod")
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("overrides must not be able to reinstate root: %+v", sc)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile must be set explicitly; Kubernetes defaults to Unconfined")
	}
	// Scheduling hints are what overrides are actually for, and they do apply.
	if pod.Spec.NodeSelector["pool"] != "sandbox" {
		t.Error("scheduling overrides should be honoured")
	}
	for _, ctr := range pod.Spec.Containers {
		if ctr.SecurityContext == nil || len(ctr.SecurityContext.Capabilities.Drop) == 0 {
			t.Errorf("container %s does not drop capabilities", ctr.Name)
		}
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a sandbox Pod is single-use; restarting it would hand a fresh namespace to a dead sandbox")
	}
}

func TestBuildPodWiresVolumesThroughToExecd(t *testing.T) {
	tpl := approvedTemplate("py312", func(x *sbxv1.SandboxTemplate) {
		x.Spec.Volumes = []sbxv1.TemplateVolume{{Name: "corpus", ClaimName: "corpus-rox", ReadOnly: true}}
	})
	l := bwrap.DefaultLayout()
	pod := BuildPod(tpl, "py312", PlatformImage{Ref: "platform@sha256:" + repeat64('b')}, l, "tok")

	var args string
	for _, a := range pod.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--volumes=") {
			args = a
		}
	}
	if !strings.Contains(args, "corpus=/var/lib/jdix/vol/corpus") {
		t.Fatalf("execd was not told where the volume is: %q", args)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "vol-corpus" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "corpus-rox" {
			found = true
		}
	}
	if !found {
		t.Error("the PVC was not attached to the Pod")
	}
}
