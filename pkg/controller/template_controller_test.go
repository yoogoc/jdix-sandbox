package controller

import (
	"context"
	"errors"
	"fmt"
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
	r := &TemplateReconciler{Client: c, Scheme: testScheme(t), Platform: testPlatform(),
		Layout: testLayout(), AllowedRegistries: registries}
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
	h := TemplateHash(base, testPlatform(), testLayout())

	// Metadata churn must not roll the pool.
	relabelled := base.DeepCopy()
	relabelled.Labels = map[string]string{"team": "search"}
	relabelled.Annotations = map[string]string{"note": "for the eval run"}
	if TemplateHash(relabelled, testPlatform(), testLayout()) != h {
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
		if TemplateHash(changed, testPlatform(), testLayout()) == h {
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
	pod := BuildPod(tpl, "py312", testPlatform(), testLayout(), "jct_x")

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
	// The profile must always be set explicitly — Kubernetes leaves it
	// Unconfined otherwise, and an accidental Unconfined is very different from
	// a deliberate one. Which profile is deliberate depends on the tier; see
	// TestBuildPodConcessionsForTheUsernsTier.
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type == "" {
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
	pod := BuildPod(tpl, "py312", testPlatform(), l, "jct_x")

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

// The userns tier does not come for free. These three settings were established
// by measuring a real cluster: without any one of them bubblewrap cannot build
// a namespace and the node degrades to the chroot tier.
func TestBuildPodConcessionsForTheUsernsTier(t *testing.T) {
	platform := testPlatform()
	l := testLayout()

	usernsTpl := approvedTemplate("py312") // minIsolationTier defaults to userns
	pod := BuildPod(usernsTpl, "py312", platform, l, "jct_x")

	if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers {
		t.Error("hostUsers must be false: Kubernetes refuses procMount Unmasked without it")
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc.ProcMount == nil || *sc.ProcMount != corev1.UnmaskedProcMount {
		t.Error("procMount must be Unmasked, or mounting a fresh /proc inside the namespace returns EPERM")
	}
	if pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Error("RuntimeDefault denies unshare(CLONE_NEWUSER) without CAP_SYS_ADMIN, which bubblewrap needs")
	}
	// Everything else stays as tight as before; only the three settings above move.
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation must stay off")
	}
	if len(sc.Capabilities.Drop) == 0 {
		t.Error("capabilities must still be dropped")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the ServiceAccount token must still never be mounted")
	}

	// A template that cannot use the userns tier gets nothing relaxed.
	chrootTpl := approvedTemplate("legacy", func(x *sbxv1.SandboxTemplate) {
		x.Spec.MinIsolationTier = sbxv1.TierChroot
	})
	weak := BuildPod(chrootTpl, "legacy", platform, l, "tok")
	if weak.Spec.HostUsers != nil {
		t.Error("hostUsers should not be set for a template that does not need it")
	}
	if weak.Spec.Containers[0].SecurityContext.ProcMount != nil {
		t.Error("procMount should be left alone for a template that does not need it")
	}
	if weak.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("a template that cannot reach the userns tier keeps RuntimeDefault")
	}
}

// ── tag resolution ──────────────────────────────────────────────────────────

// fakeResolver stands in for a registry.
type fakeResolver struct {
	calls  []string
	digest string
	bytes  int64
	err    error
}

func (f *fakeResolver) Resolve(_ context.Context, ref string, secrets []string, ns string) (ResolvedImage, error) {
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return ResolvedImage{}, f.err
	}
	d := f.digest
	if d == "" {
		d = "sha256:" + repeat64('d')
	}
	return ResolvedImage{Digest: d, Bytes: f.bytes}, nil
}

func reconcileWithResolver(t *testing.T, c client.Client, tpl *sbxv1.SandboxTemplate, r *TemplateReconciler) (*sbxv1.SandboxTemplate, ctrl.Result) {
	t.Helper()
	r.Client, r.Scheme = c, testScheme(t)
	r.Platform, r.Layout = testPlatform(), testLayout()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: tpl.Namespace, Name: tpl.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got sbxv1.SandboxTemplate
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tpl.Namespace, Name: tpl.Name}, &got); err != nil {
		t.Fatal(err)
	}
	return &got, res
}

// A template written the way anyone would write one — with a tag — must work.
// Demanding a digest pushes work onto the tenant that they cannot do until
// after a push, and the platform is the one that can do it.
func TestATagIsResolvedAndPinned(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/jdix/py312:2026-09-01")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{digest: "sha256:" + repeat64('e'), bytes: 200 << 20}

	got, _ := reconcileWithResolver(t, c, tpl, &TemplateReconciler{
		AllowedRegistries: []string{"registry.internal"}, Resolver: fr,
	})

	if got.Status.Admission != sbxv1.AdmissionApproved {
		t.Fatalf("admission %q reason %q", got.Status.Admission, got.Status.AdmissionReason)
	}
	if got.Status.ResolvedDigest != "sha256:"+repeat64('e') {
		t.Fatalf("digest %q", got.Status.ResolvedDigest)
	}
	if got.Status.ResolvedRef != "registry.internal/jdix/py312:2026-09-01" {
		t.Errorf("ResolvedRef should record what was resolved: %q", got.Status.ResolvedRef)
	}
	if got.Status.ResolvedAt == nil || got.Status.ImageBytes != 200<<20 {
		t.Errorf("status is incomplete: %+v", got.Status)
	}

	// What Pods run is the digest, not the tag.
	if ref := ImageRef(got); ref != "registry.internal/jdix/py312@sha256:"+repeat64('e') {
		t.Fatalf("Pods would run %q", ref)
	}
}

func TestAResolvedTemplateIsNotResolvedAgain(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{}
	r := &TemplateReconciler{AllowedRegistries: []string{"registry.internal"}, Resolver: fr}

	got, _ := reconcileWithResolver(t, c, tpl, r)
	reconcileWithResolver(t, c, got, r)
	reconcileWithResolver(t, c, got, r)

	if len(fr.calls) != 1 {
		t.Fatalf("resolved %d times; a settled template must not keep hitting the registry", len(fr.calls))
	}
}

// Changing the tag has to re-resolve, or an update would never take effect.
func TestChangingTheTagReResolvesAndRollsThePool(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{digest: "sha256:" + repeat64('a')}
	r := &TemplateReconciler{AllowedRegistries: []string{"registry.internal"}, Resolver: fr}

	first, _ := reconcileWithResolver(t, c, tpl, r)
	hashBefore := first.Status.Hash

	first.Spec.Image.Ref = "registry.internal/x:v2"
	if err := c.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	fr.digest = "sha256:" + repeat64('b')
	second, _ := reconcileWithResolver(t, c, first, r)

	if len(fr.calls) != 2 {
		t.Fatalf("a changed tag must be re-resolved; calls: %v", fr.calls)
	}
	if second.Status.ResolvedDigest != "sha256:"+repeat64('b') {
		t.Fatalf("digest %q", second.Status.ResolvedDigest)
	}
	// The hash is what makes warm Pods stale; without this the pool would go on
	// serving the old build.
	if second.Status.Hash == hashBefore {
		t.Fatal("the template hash did not change, so the warm pool would never roll")
	}
}

// A registry outage is not the template's fault. Rejecting on one would need a
// human to resubmit a template that was always correct.
func TestATransientRegistryFailureIsRetriedNotRejected(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{err: errors.New("dial tcp: connection refused")}
	r := &TemplateReconciler{AllowedRegistries: []string{"registry.internal"}, Resolver: fr}

	got, res := reconcileWithResolver(t, c, tpl, r)
	if got.Status.Admission == sbxv1.AdmissionRejected {
		t.Fatal("a transient failure must not reject the template")
	}
	if res.RequeueAfter <= 0 {
		t.Fatal("it must be retried")
	}
	if !strings.Contains(got.Status.AdmissionReason, "connection refused") {
		t.Errorf("the reason should say what went wrong: %q", got.Status.AdmissionReason)
	}

	// And once the registry comes back, it settles.
	fr.err = nil
	after, res2 := reconcileWithResolver(t, c, got, r)
	if after.Status.Admission != sbxv1.AdmissionApproved || res2.RequeueAfter != 0 {
		t.Fatalf("admission %q requeue %v", after.Status.Admission, res2.RequeueAfter)
	}
}

// A missing image is the template's fault, and stays rejected.
func TestAMissingImageIsRejected(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/nope:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{err: fmt.Errorf("%w: registry.internal/nope:v1", ErrImageNotFound)}
	r := &TemplateReconciler{AllowedRegistries: []string{"registry.internal"}, Resolver: fr}

	got, res := reconcileWithResolver(t, c, tpl, r)
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q", got.Status.Admission)
	}
	if res.RequeueAfter != 0 {
		t.Error("a missing image should not be retried on a timer")
	}
	if !strings.Contains(got.Status.AdmissionReason, "pull secret") {
		t.Errorf("the reason should hint at the usual cause: %q", got.Status.AdmissionReason)
	}
}

// An already-pinned reference needs no registry at all. This is the path an
// air-gapped install takes, and the one hack/dev/up.sh uses for an image that
// was built locally and never pushed.
func TestAPinnedReferenceSkipsTheRegistry(t *testing.T) {
	ref := "registry.internal/x@sha256:" + repeat64('c')
	tpl := rawTemplate("py312", ref)
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{}

	got, _ := reconcileWithResolver(t, c, tpl, &TemplateReconciler{
		AllowedRegistries: []string{"registry.internal"}, Resolver: fr,
	})
	if len(fr.calls) != 0 {
		t.Fatal("a pinned reference must not be sent to a registry")
	}
	if got.Status.Admission != sbxv1.AdmissionApproved || got.Status.ResolvedDigest != "sha256:"+repeat64('c') {
		t.Fatalf("status: %+v", got.Status)
	}
	if ImageRef(got) != ref {
		t.Errorf("ImageRef changed a pinned reference: %q", ImageRef(got))
	}
}

// Without a resolver — an air-gapped install — a tag cannot be honoured, and
// the message has to say so rather than failing mysteriously later.
func TestWithoutAResolverATagIsRejectedWithAdvice(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)

	got, _ := reconcileWithResolver(t, c, tpl, &TemplateReconciler{
		AllowedRegistries: []string{"registry.internal"},
	})
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q", got.Status.Admission)
	}
	if !strings.Contains(got.Status.AdmissionReason, "name@sha256:") {
		t.Errorf("the message should say exactly what to write: %q", got.Status.AdmissionReason)
	}
}

func TestAnOversizedImageIsRejected(t *testing.T) {
	tpl := rawTemplate("py312", "registry.internal/huge:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{bytes: 9 << 30}

	got, _ := reconcileWithResolver(t, c, tpl, &TemplateReconciler{
		AllowedRegistries: []string{"registry.internal"}, Resolver: fr,
		MaxImageBytes: 5 << 30,
	})
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q", got.Status.Admission)
	}
	if !strings.Contains(got.Status.AdmissionReason, "GiB") {
		t.Errorf("the reason should state the actual size: %q", got.Status.AdmissionReason)
	}
}

func TestPinnedRewritesTagsAndLeavesDigestsAlone(t *testing.T) {
	d := "sha256:" + repeat64('f')
	cases := map[string]string{
		"reg.io/x:v1":      "reg.io/x@" + d,
		"reg.io/x":         "reg.io/x@" + d,
		"reg.io:5000/x:v1": "reg.io:5000/x@" + d,
		"reg.io/x@" + d:    "reg.io/x@" + d,
	}
	for in, want := range cases {
		if got := pinned(in, d); got != want {
			t.Errorf("pinned(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── opting out of resolution ────────────────────────────────────────────────

// Some images the platform simply cannot see: built locally and never pushed,
// or behind a registry only the nodes can reach. Such a template must still be
// usable, with the kubelet pulling by tag when a Pod starts.
func TestATemplateCanOptOutOfResolution(t *testing.T) {
	tpl := rawTemplate("local", "jdix/sandbox-base:dev", func(x *sbxv1.SandboxTemplate) {
		x.Spec.Image.Resolve = ptr(false)
	})
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{}

	got, res := reconcileWithResolver(t, c, tpl, &TemplateReconciler{Resolver: fr})

	if got.Status.Admission != sbxv1.AdmissionApproved {
		t.Fatalf("admission %q reason %q", got.Status.Admission, got.Status.AdmissionReason)
	}
	if len(fr.calls) != 0 {
		t.Fatal("the registry must not be contacted for a template that opted out")
	}
	if res.RequeueAfter != 0 {
		t.Error("nothing is pending, so nothing should be retried")
	}
	// Nothing is pinned, and status says so rather than leaving it to be
	// inferred from an empty digest.
	if got.Status.ImagePinned {
		t.Error("ImagePinned must be false; it is the answer to 'why is this pool running two builds'")
	}
	if got.Status.ResolvedDigest != "" {
		t.Errorf("ResolvedDigest should stay empty, got %q", got.Status.ResolvedDigest)
	}
	// Pods run exactly what the template said.
	if ref := ImageRef(got); ref != "jdix/sandbox-base:dev" {
		t.Fatalf("Pods would run %q, expected the tag verbatim", ref)
	}
}

// Turning resolution off after it happened must clear the stale digest, or Pods
// would keep running an image the template no longer names.
func TestTurningResolutionOffClearsThePin(t *testing.T) {
	tpl := rawTemplate("x", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)
	fr := &fakeResolver{digest: "sha256:" + repeat64('a')}
	r := &TemplateReconciler{AllowedRegistries: []string{"registry.internal"}, Resolver: fr}

	pinned, _ := reconcileWithResolver(t, c, tpl, r)
	if !pinned.Status.ImagePinned {
		t.Fatal("expected the first pass to pin")
	}

	pinned.Spec.Image.Resolve = ptr(false)
	if err := c.Update(context.Background(), pinned); err != nil {
		t.Fatal(err)
	}
	after, _ := reconcileWithResolver(t, c, pinned, r)

	if after.Status.ResolvedDigest != "" || after.Status.ImagePinned {
		t.Fatalf("a stale pin survived: digest=%q pinned=%v",
			after.Status.ResolvedDigest, after.Status.ImagePinned)
	}
	if ImageRef(after) != "registry.internal/x:v1" {
		t.Fatalf("Pods would run %q", ImageRef(after))
	}
}

// Without a resolver, the error has to name both ways out — pinning by hand and
// opting out — or the reader is left guessing which one applies to them.
func TestTheNoResolverMessageOffersBothWaysOut(t *testing.T) {
	tpl := rawTemplate("x", "registry.internal/x:v1")
	c := newFakeClient(t, tpl)

	got, _ := reconcileWithResolver(t, c, tpl, &TemplateReconciler{
		AllowedRegistries: []string{"registry.internal"},
	})
	if got.Status.Admission != sbxv1.AdmissionRejected {
		t.Fatalf("admission %q", got.Status.Admission)
	}
	for _, want := range []string{"name@sha256:", "resolve=false"} {
		if !strings.Contains(got.Status.AdmissionReason, want) {
			t.Errorf("the message should mention %q: %q", want, got.Status.AdmissionReason)
		}
	}
}

func TestPullPolicy(t *testing.T) {
	platform := testPlatform()
	l := testLayout()

	// Default: IfNotPresent, whether pinned or not.
	def := approvedTemplate("py312")
	if p := BuildPod(def, "p", platform, l, "tok").Spec.Containers[0].ImagePullPolicy; p != corev1.PullIfNotPresent {
		t.Errorf("default pull policy %q", p)
	}

	// Never is what a node-local image needs, and is the reason this is
	// configurable at all.
	local := approvedTemplate("local", func(x *sbxv1.SandboxTemplate) {
		x.Spec.Image.Resolve = ptr(false)
		x.Spec.Image.PullPolicy = corev1.PullNever
	})
	if p := BuildPod(local, "p", platform, l, "tok").Spec.Containers[0].ImagePullPolicy; p != corev1.PullNever {
		t.Errorf("pull policy %q, want Never", p)
	}
}

// The hash still has to track the tag, or editing the template would never roll
// the pool. What it cannot track is the tag being repointed underneath — that is
// the cost of opting out, and it is stated on the field.
func TestAnUnpinnedTemplateStillRollsWhenTheTagIsEdited(t *testing.T) {
	a := approvedTemplate("x", func(t *sbxv1.SandboxTemplate) {
		t.Spec.Image.Ref = "reg.io/x:v1"
		t.Spec.Image.Resolve = ptr(false)
	})
	b := a.DeepCopy()
	b.Spec.Image.Ref = "reg.io/x:v2"

	if TemplateHash(a, testPlatform(), testLayout()) == TemplateHash(b, testPlatform(), testLayout()) {
		t.Fatal("editing the tag must change the hash, or a template update would never take effect")
	}
}
