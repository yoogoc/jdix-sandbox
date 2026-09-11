package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

// Each Pod carries its own control-plane credential. A shared one would mean
// that escaping any single sandbox yields the key to every other Pod's control
// plane — the one thing that plane exists to prevent.
func TestEachPodGetsItsOwnControlToken(t *testing.T) {
	tpl := approvedTemplate("py312")
	platform := testPlatform()
	l := testLayout()

	a := BuildPod(tpl, "p", platform, l, NewControlToken())
	b := BuildPod(tpl, "p", platform, l, NewControlToken())

	ta, tb := PodToken(a), PodToken(b)
	if ta == "" || tb == "" {
		t.Fatalf("tokens missing: %q %q", ta, tb)
	}
	if ta == tb {
		t.Fatal("two Pods were given the same token")
	}
	if len(ta) < 32 {
		t.Errorf("token looks too short to be unguessable: %q", ta)
	}

	// It travels in the spec, not in a Secret: there is then no shared object
	// that has to exist, with a matching value, in every tenant namespace.
	for _, v := range a.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("unexpected Secret volume %q; the token should not need one", v.Name)
		}
	}
	// And execd must be able to find it without being told where.
	for _, arg := range a.Spec.Containers[0].Args {
		if len(arg) > 21 && arg[:21] == "--internal-token-file" {
			t.Error("execd should read the token from the environment, not a mounted file")
		}
	}
}

func TestPodTokenReadsBackWhatWasSet(t *testing.T) {
	tpl := approvedTemplate("py312")
	pod := BuildPod(tpl, "p", PlatformImage{Ref: "x@sha256:" + repeat64('b')}, bwrap.DefaultLayout(), "jct_known")
	if got := PodToken(pod); got != "jct_known" {
		t.Fatalf("PodToken = %q", got)
	}
	// A Pod without one reads as empty rather than panicking; the reconciler
	// then sends no credential and execd refuses, which is the safe direction.
	if got := PodToken(&corev1.Pod{}); got != "" {
		t.Fatalf("PodToken on an empty Pod = %q", got)
	}
}

// The reconciler must authenticate with the credential belonging to the Pod it
// actually claimed, not with anything global.
func TestBindUsesTheClaimedPodsOwnToken(t *testing.T) {
	tpl := approvedTemplate("py312")
	warm := warmPod("warm-1", "py312", tpl.Status.Hash, string(bwrap.TierUserns), true)
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, warm, sbx)
	b := &fakeBinder{}
	r := newSandboxReconciler(t, c, b)

	drive(t, r, c, "sbx-1", 6)

	if len(b.seenTokens) != 1 {
		t.Fatalf("expected one bind, got %d", len(b.seenTokens))
	}
	// warmPod() stamps each Pod with jct_<name>.
	if b.seenTokens[0] != "jct_warm-1" {
		t.Fatalf("bound with %q, expected the claimed Pod's own token", b.seenTokens[0])
	}
}

func TestAColdStartedPodGetsAToken(t *testing.T) {
	tpl := approvedTemplate("py312")
	sbx := newSandbox("sbx-1", "py312")
	c := newFakeClient(t, tpl, sbx)
	r := newSandboxReconciler(t, c, &fakeBinder{})

	step(t, r, "sbx-1", 2)

	pods := listPods(t, c)
	if len(pods) != 1 {
		t.Fatalf("expected one Pod, got %d", len(pods))
	}
	if PodToken(&pods[0]) != "jct_deterministic" {
		t.Fatalf("cold-started Pod token = %q", PodToken(&pods[0]))
	}
}

// Two Pods supplied into the same pool must not share a credential either.
func TestPoolSuppliesDistinctTokens(t *testing.T) {
	tpl := approvedTemplate("py312")
	pool := newPool("py312", "py312", 3)
	c := newFakeClient(t, tpl, pool)
	r := newPoolReconciler(t, c, &fakeBinder{})
	r.NewToken = nil // use the real generator

	reconcilePool(t, r, "py312")

	seen := map[string]bool{}
	for _, p := range poolPods(t, c, "py312") {
		tok := PodToken(&p)
		if tok == "" {
			t.Fatalf("Pod %s has no token", p.Name)
		}
		if seen[tok] {
			t.Fatalf("token %q was reused across Pods", tok)
		}
		seen[tok] = true
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct tokens, want 3", len(seen))
	}
}

// The installer copies the platform tree out of its own image and into the
// volume the sandbox container will see. That only works if the volume is
// staged somewhere else: mounting it over PlatformRoot hides the very files
// being copied, and the command silently becomes a copy of an empty directory
// onto itself. This is exactly what shipped once, and it failed at runtime with
// "cp: cannot stat '/opt/jdix/bin/.'".
func TestInstallerDoesNotMountOverItsOwnSource(t *testing.T) {
	pod := BuildPod(approvedTemplate("py312"), "p",
		testPlatform(), testLayout(), "jct_x")

	init := pod.Spec.InitContainers[0]
	for _, m := range init.VolumeMounts {
		if m.MountPath == bwrap.PlatformRoot ||
			strings.HasPrefix(bwrap.PlatformRoot, m.MountPath+"/") {
			t.Fatalf("installer mounts %q over its source %q; the copy would find nothing",
				m.MountPath, bwrap.PlatformRoot)
		}
	}

	// The sandbox container, by contrast, must see the volume at PlatformRoot —
	// that is the path everything downstream is built around.
	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "jdix-bin" && m.MountPath == bwrap.PlatformRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("the sandbox container does not see the platform volume at %s", bwrap.PlatformRoot)
	}

	// Both containers must be talking about the same volume, or the copy lands
	// somewhere nothing reads.
	initVol, mainVol := "", ""
	for _, m := range init.VolumeMounts {
		if m.MountPath == installStagingPath {
			initVol = m.Name
		}
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == bwrap.PlatformRoot {
			mainVol = m.Name
		}
	}
	if initVol == "" || initVol != mainVol {
		t.Fatalf("staging volume %q and runtime volume %q are not the same", initVol, mainVol)
	}
}

// bwrap is dynamically linked and runs inside a tenant image that may have
// neither its loader nor its libraries, so lib/ has to travel with bin/.
// Copying only bin/ leaves a wrapper script pointing at a loader that is not
// there.
func TestInstallerCopiesTheWholePlatformTree(t *testing.T) {
	pod := BuildPod(approvedTemplate("py312"), "p",
		testPlatform(), testLayout(), "jct_x")

	script := strings.Join(pod.Spec.InitContainers[0].Args, " ")
	if !strings.Contains(script, bwrap.PlatformRoot+"/* ") {
		t.Fatalf("the installer should copy every entry of the tree:\n  %s", script)
	}
	if strings.Contains(script, bwrap.PlatformRoot+"/bin/.") {
		t.Fatalf("copying only bin/ leaves bubblewrap without its libraries:\n  %s", script)
	}
	// "cp -a src/. dst/" applies the source's attributes to dst, which is the
	// volume root; the installer does not own it and the copy fails with EPERM.
	if strings.Contains(script, bwrap.PlatformRoot+"/. ") {
		t.Fatalf("copying the directory itself fails on the volume root's timestamps:\n  %s", script)
	}
	// A failure partway through must not leave a half-installed volume looking
	// like a successful one.
	if !strings.Contains(script, "set -e") {
		t.Errorf("the installer script should abort on the first failure:\n  %s", script)
	}
	if !strings.Contains(script, "test -x "+installStagingPath+"/bin/jdix-execd") {
		t.Errorf("the installer should assert it produced something usable:\n  %s", script)
	}
}

// The hash exists so a stale warm Pod gets replaced. It used to cover a
// hand-picked list of template fields, which meant a changed platform image —
// or a change to this file — left every existing Pod looking current. The pool
// then kept serving Pods built by an older controller, and every bind that
// landed on one failed with a 401 that pointed nowhere near the cause.
func TestTheHashTracksThePodAndNotJustTheTemplate(t *testing.T) {
	tpl := approvedTemplate("py312")
	base := TemplateHash(tpl, testPlatform(), testLayout())

	// A different platform image produces different Pods, so it must roll.
	other := PlatformImage{Ref: "registry.internal/jdix/platform@sha256:" + repeat64('c')}
	if TemplateHash(tpl, other, testLayout()) == base {
		t.Error("changing the platform image left the hash unchanged; existing warm Pods would never be replaced")
	}

	// So does a different layout.
	l := testLayout()
	l.WorkspaceDir = "/var/lib/jdix/other"
	if TemplateHash(tpl, testPlatform(), l) == base {
		t.Error("changing the layout left the hash unchanged")
	}
}

// The hash must be stable, or every Pod would be born stale. The per-Pod token
// is random, so it has to be excluded — this is what that exclusion protects.
func TestTheHashIsStableAcrossCalls(t *testing.T) {
	tpl := approvedTemplate("py312")
	first := TemplateHash(tpl, testPlatform(), testLayout())
	for i := 0; i < 5; i++ {
		if got := TemplateHash(tpl, testPlatform(), testLayout()); got != first {
			t.Fatalf("hash is not deterministic: %q then %q", first, got)
		}
	}
	// And a Pod built from that template carries the same value, so it does not
	// look stale the instant it is created.
	pod := BuildPod(tpl, "p", testPlatform(), testLayout(), NewControlToken())
	if pod.Labels[sbxv1.LabelTemplateHash] != first {
		t.Fatalf("a fresh Pod is labelled %q but the template hashes to %q",
			pod.Labels[sbxv1.LabelTemplateHash], first)
	}
}

// The whole point of the new mode: the Pod stops asking for a user namespace,
// so the runtime stops trying to idmap its volumes, so an NFS-backed PVC can be
// mounted at all (docs/NFS-CSI-FILESYSTEM-ISOLATION.md §2).
func TestFilesystemModePodDropsTheUserNamespace(t *testing.T) {
	tpl := approvedTemplate("fs")
	tpl.Spec.MinIsolationTier = ""
	tpl.Spec.FilesystemIsolation = "bwrap"
	pod := BuildPod(tpl, "", PlatformImage{Ref: "img"}, bwrap.DefaultLayout(), "tok")

	if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers != true {
		t.Errorf("hostUsers = %v; false is what makes the runtime idmap every volume", pod.Spec.HostUsers)
	}
	if pm := pod.Spec.Containers[0].SecurityContext.ProcMount; pm != nil && *pm != corev1.DefaultProcMount {
		t.Errorf("procMount = %v; Unmasked is what requires hostUsers=false", *pm)
	}
	sc := pod.Spec.SecurityContext.SeccompProfile
	if sc == nil || sc.Type != corev1.SeccompProfileTypeLocalhost {
		t.Errorf("seccomp = %+v; the setup phase needs a targeted profile, never Unconfined", sc)
	}
	var told bool
	for _, a := range pod.Spec.Containers[0].Args {
		if a == "--filesystem-isolation=bwrap" {
			told = true
		}
	}
	if !told {
		t.Error("execd was not told which mode to probe for")
	}
}

// A legacy template must keep its old Pod shape. Reinterpreting it as the new
// mode would silently swap one isolation model for another under a running
// pool (docs §7).
func TestLegacyTemplateKeepsItsPodShape(t *testing.T) {
	tpl := approvedTemplate("legacy")
	tpl.Spec.MinIsolationTier = sbxv1.TierUserns
	pod := BuildPod(tpl, "", PlatformImage{Ref: "img"}, bwrap.DefaultLayout(), "tok")

	if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers != false {
		t.Errorf("hostUsers = %v, want false for the legacy userns tier", pod.Spec.HostUsers)
	}
	if pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Error("the legacy tier's seccomp posture changed")
	}
}

// The two ways of asking are mutually exclusive, and a template that asks both
// ways is rejected rather than resolved in some order the author cannot see.
func TestFilesystemModeAndLegacyTierAreExclusive(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*sbxv1.SandboxTemplate)
	}{
		{"both", func(x *sbxv1.SandboxTemplate) {
			x.Spec.FilesystemIsolation = "bwrap"
			x.Spec.MinIsolationTier = sbxv1.TierUserns
		}},
		{"podUserNamespace without the mode", func(x *sbxv1.SandboxTemplate) {
			v := true
			x.Spec.PodUserNamespace = &v
		}},
		{"filesystem as a legacy tier", func(x *sbxv1.SandboxTemplate) {
			x.Spec.MinIsolationTier = sbxv1.TierFilesystem
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpl := rawTemplate("x", "registry.internal/x@sha256:"+repeat64('a'), tc.mod)
			got := reconcileTemplate(t, newFakeClient(t, tpl), tpl, "registry.internal")
			if got.Status.Admission != sbxv1.AdmissionRejected {
				t.Fatalf("admission %q (%s), want Rejected",
					got.Status.Admission, got.Status.AdmissionReason)
			}
		})
	}
}
