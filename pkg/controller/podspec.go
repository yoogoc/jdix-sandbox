// Package controller reconciles Sandbox, SandboxTemplate and SandboxPool.
package controller

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
	"jdix.io/sandbox/pkg/bwrap"
)

// PlatformImage carries execd, jdix-init and bubblewrap. An initContainer copies
// them into an emptyDir, so a tenant's own image needs no cooperation from us.
type PlatformImage struct {
	Ref        string
	PullPolicy corev1.PullPolicy
}

// Ports the sandbox Pod listens on.
const (
	PortData    int32 = 8080
	PortControl int32 = 8081
)

// TemplateHash fingerprints the warm Pod a template produces.
//
// Warm Pods carry it as a label, and that is the whole mechanism behind rolling
// a pool: a Pod whose hash no longer matches is stale and gets replaced.
//
// It hashes the rendered Pod spec rather than a hand-picked list of fields.
// Picking fields means remembering to add each new one, and the consequence of
// forgetting is invisible: the pool keeps serving Pods built by an older
// version, and every bind that lands on one fails for reasons that point
// somewhere else entirely. Hashing the output makes a changed platform image,
// a changed layout, or a change to this file roll the pool on its own.
//
// The per-Pod token is blanked first: it is random, and a hash that changed on
// every call would mark every Pod stale the moment it was created.
func TemplateHash(t *sbxv1.SandboxTemplate, platform PlatformImage, l bwrap.Layout) string {
	b, err := json.Marshal(buildPodSpec(t, platform, l, ""))
	if err != nil {
		// Marshalling a PodSpec cannot fail; if it somehow did, a hash that
		// changes every time is safer than one that never does.
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// volumeMountPath is where a template volume lands inside the Pod. Sandboxes
// only ever see subpaths of it, never the root.
func volumeMountPath(l bwrap.Layout, v sbxv1.TemplateVolume) string {
	if v.MountPath != "" {
		return v.MountPath
	}
	return path.Join(l.VolumeRoot, v.Name)
}

// VolumeArgs renders the --volumes flag execd uses to validate spec.mounts.
func VolumeArgs(l bwrap.Layout, t *sbxv1.SandboxTemplate) string {
	out := ""
	for i, v := range t.Spec.Volumes {
		if i > 0 {
			out += ","
		}
		out += v.Name + "=" + volumeMountPath(l, v)
	}
	return out
}

// BuildPod renders a warm Pod for a template.
//
// It is created in the idle state with no sandbox identity: a Pod becomes a
// particular tenant's only when the bind swaps its labels.
// ControlTokenEnv is where each Pod finds its own control-plane credential.
//
// The token travels in the Pod spec rather than in a Secret. That keeps it to
// one Pod — an escaped sandbox learns nothing about any other — and removes the
// shared Secret that would otherwise have to exist, with a matching value, in
// every tenant namespace. The cost is that anyone able to read Pods in the
// namespace can read it; that reader already has API access, whereas the
// attacker this defends against has none.
const ControlTokenEnv = "JDIX_CONTROL_TOKEN"

// BuildPod renders a warm Pod. token is that Pod's own control-plane
// credential; give every Pod a different one.
func BuildPod(t *sbxv1.SandboxTemplate, pool string, platform PlatformImage, l bwrap.Layout, token string) *corev1.Pod {
	hash := TemplateHash(t, platform, l)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: t.Name + "-" + hash + "-",
			Namespace:    t.Namespace,
			Labels: map[string]string{
				sbxv1.LabelPool:         pool,
				sbxv1.LabelTemplate:     t.Name,
				sbxv1.LabelTemplateHash: hash,
				sbxv1.LabelState:        sbxv1.StateIdle,
				sbxv1.LabelManagedBy:    sbxv1.ManagedBy,
			},
		},
		Spec: buildPodSpec(t, platform, l, token),
	}
}

// buildPodSpec renders the spec alone, so TemplateHash can fingerprint it
// without calling BuildPod and recursing through the hash label.
func buildPodSpec(t *sbxv1.SandboxTemplate, platform PlatformImage, l bwrap.Layout, token string) corev1.PodSpec {
	nonRoot := true
	noEscalate := false
	uid := int64(1000)

	// Reaching the userns tier costs three specific concessions, measured on a
	// real cluster rather than assumed:
	//
	//   hostUsers: false      the Pod gets its own user namespace. Without it
	//                         Kubernetes refuses procMount: Unmasked outright.
	//   procMount: Unmasked   Kubernetes masks parts of /proc by bind-mounting
	//                         over them. bubblewrap then cannot mount a fresh
	//                         procfs inside its namespace — the kernel's
	//                         mount_too_revealing() check returns EPERM,
	//                         because the new mount would uncover what the
	//                         runtime deliberately hid.
	//   seccomp: Unconfined   the RuntimeDefault profile denies unshare with
	//                         CLONE_NEWUSER to containers without
	//                         CAP_SYS_ADMIN, which is precisely the call
	//                         bubblewrap has to make.
	//
	// Relaxing seccomp is a real cost, and it is paid for by hostUsers: false —
	// the container is inside a user namespace mapped to unprivileged host ids,
	// so the syscalls seccomp would have blocked no longer carry authority over
	// the host. A narrow custom profile (RuntimeDefault plus CLONE_NEWUSER)
	// would be strictly better and is the obvious next step.
	//
	// Templates that do not ask for the userns tier keep RuntimeDefault and get
	// the weaker chroot tier; nothing is relaxed for a workload that cannot use
	// it.
	wantsUserns := t.Spec.MinIsolationTier == sbxv1.TierUserns
	seccomp := corev1.SeccompProfileTypeRuntimeDefault
	if wantsUserns {
		seccomp = corev1.SeccompProfileTypeUnconfined
	}

	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// The sandbox must not be able to reach the Kubernetes API even if it
		// escapes bubblewrap, so the token is never mounted in the first place.
		AutomountServiceAccountToken: ptr(false),
		RuntimeClassName:             optionalString(t.Spec.RuntimeClassName),
		// Terminating promptly matters: a Pod that lingers holds pool capacity.
		TerminationGracePeriodSeconds: ptr(int64(10)),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &nonRoot,
			RunAsUser:      &uid,
			RunAsGroup:     &uid,
			FSGroup:        &uid,
			SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
		},
		InitContainers: []corev1.Container{{
			Name:            "jdix-installer",
			Image:           platform.Ref,
			ImagePullPolicy: platform.PullPolicy,
			Command:         []string{"/bin/sh", "-c"},
			Args: []string{
				// Copy rather than mount: the tenant image keeps its own /usr,
				// and the platform binaries have to live somewhere it cannot
				// write.
				//
				// The whole tree goes, not just bin/. lib/ carries bubblewrap's
				// loader and shared libraries, because bwrap has to run inside a
				// tenant image that may have neither.
				//
				// The entries are copied rather than the directory itself:
				// "cp -a src/. dst/" applies the source's attributes to dst, and
				// dst is the volume root, owned by root with only group write
				// from fsGroup. Setting timestamps needs ownership, not write
				// access, so that form fails with EPERM.
				//
				// The final test is the success condition made explicit. A
				// half-copied volume would otherwise look like a working one
				// until execd failed to start for reasons that point nowhere
				// near here.
				fmt.Sprintf("set -e; cp -a %s/* %s/; mkdir -p %s %s %s; chmod 700 %s; test -x %s/bin/jdix-execd",
					bwrap.PlatformRoot, installStagingPath,
					l.WorkspaceDir, l.VolumeRoot, l.IPCDir, l.IPCDir,
					installStagingPath),
			},
			VolumeMounts: installerMounts(l),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noEscalate,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Containers: []corev1.Container{{
			Name:            "sandbox",
			Image:           ImageRef(t),
			ImagePullPolicy: pullPolicyFor(t),
			Command:         []string{path.Join(bwrap.PlatformRoot, "bin", "jdix-execd")},
			Args: []string{
				fmt.Sprintf("--data-addr=:%d", PortData),
				fmt.Sprintf("--control-addr=:%d", PortControl),
				"--bwrap=" + path.Join(bwrap.PlatformRoot, "bin", "bwrap"),
				"--volumes=" + VolumeArgs(l, t),
			},
			Env: []corev1.EnvVar{{Name: ControlTokenEnv, Value: token}},
			Ports: []corev1.ContainerPort{
				{Name: "data", ContainerPort: PortData},
				{Name: "control", ContainerPort: PortControl},
			},
			Resources:    t.Spec.Resources,
			VolumeMounts: append(platformMounts(l), templateVolumeMounts(l, t)...),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noEscalate,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				ProcMount:                procMountFor(wantsUserns),
			},
			// Readiness gates pool membership. A Pod whose node cannot meet the
			// template's tier never reports ready, so the failure lands on the
			// supply path instead of on a user's request.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/internal/v1/probe",
						Port: intstrFromInt(PortControl),
					},
				},
				InitialDelaySeconds: 1,
				PeriodSeconds:       2,
				FailureThreshold:    30,
			},
		}},
		Volumes: podVolumes(l, t),
	}
	if wantsUserns {
		spec.HostUsers = ptr(false)
	}

	if t.Spec.Image.PullSecretRef != nil {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{*t.Spec.Image.PullSecretRef}
	}
	if t.Spec.PodOverrides != nil {
		mergeOverrides(&spec, t.Spec.PodOverrides)
	}
	return spec
}

// mergeOverrides applies the template's escape hatch and then puts back
// everything that carries a security guarantee.
//
// Overrides exist for scheduling and sidecars, not for turning the boundary
// off, so the fields below are re-applied unconditionally after the merge.
func mergeOverrides(spec *corev1.PodSpec, o *corev1.PodSpec) {
	if o.NodeSelector != nil {
		spec.NodeSelector = o.NodeSelector
	}
	if o.Tolerations != nil {
		spec.Tolerations = o.Tolerations
	}
	if o.Affinity != nil {
		spec.Affinity = o.Affinity
	}
	if o.PriorityClassName != "" {
		spec.PriorityClassName = o.PriorityClassName
	}
	if o.SchedulerName != "" {
		spec.SchedulerName = o.SchedulerName
	}
	// Everything else is ignored on purpose. In particular securityContext,
	// automountServiceAccountToken, hostNetwork, hostPID and volumes stay under
	// the controller's control.
}

// installStagingPath is where the installer sees the shared volume.
//
// It cannot be PlatformRoot. The installer runs the platform image, whose own
// /opt/jdix holds the very files being copied; mounting an empty volume there
// would hide the source, and the copy would become "cp /opt/jdix/. /opt/jdix/"
// against an empty directory. Staging elsewhere keeps source and destination
// distinct.
const installStagingPath = "/mnt/jdix"

// installerMounts are the initContainer's mounts: the shared volume staged out
// of the way, plus the state directory it seeds.
func installerMounts(l bwrap.Layout) []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "jdix-bin", MountPath: installStagingPath},
		{Name: "jdix-state", MountPath: bwrap.StateRoot},
	}
}

// platformMounts are the sandbox container's mounts, where the same volume
// appears at the path everything else expects.
func platformMounts(l bwrap.Layout) []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "jdix-bin", MountPath: bwrap.PlatformRoot},
		{Name: "jdix-state", MountPath: bwrap.StateRoot},
	}
}

func templateVolumeMounts(l bwrap.Layout, t *sbxv1.SandboxTemplate) []corev1.VolumeMount {
	out := make([]corev1.VolumeMount, 0, len(t.Spec.Volumes)+1)
	for _, v := range t.Spec.Volumes {
		out = append(out, corev1.VolumeMount{
			Name:      "vol-" + v.Name,
			MountPath: volumeMountPath(l, v),
			ReadOnly:  v.ReadOnly,
		})
	}
	return out
}

func podVolumes(l bwrap.Layout, t *sbxv1.SandboxTemplate) []corev1.Volume {
	sizeLimit := t.Spec.FilesystemDefaults.Workspace.SizeLimit
	if sizeLimit == "" {
		sizeLimit = "2Gi"
	}
	q, err := resource.ParseQuantity(sizeLimit)
	var limit *resource.Quantity
	if err == nil {
		limit = &q
	}

	vols := []corev1.Volume{
		{Name: "jdix-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "jdix-state", VolumeSource: corev1.VolumeSource{
			// The workspace lives here, so the size limit is what stops a
			// sandbox from filling the node's disk.
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: limit},
		}},
	}
	for _, v := range t.Spec.Volumes {
		vols = append(vols, corev1.Volume{
			Name: "vol-" + v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.ClaimName,
					ReadOnly:  v.ReadOnly,
				},
			},
		})
	}
	return vols
}

// procMountFor asks for an unmasked /proc only where it is both needed and
// permitted; Kubernetes rejects the Pod otherwise.
func procMountFor(wantsUserns bool) *corev1.ProcMountType {
	if !wantsUserns {
		return nil
	}
	m := corev1.UnmaskedProcMount
	return &m
}

// pullPolicyFor decides how the kubelet fetches the image.
//
// IfNotPresent is the default in both cases, but for different reasons: a
// digest cannot change, so caching it is free; an unresolved tag is cached
// per-node, which is a source of drift the template accepted when it turned
// resolution off. A template that cares can say Always, or Never for an image
// that only exists on the node.
func pullPolicyFor(t *sbxv1.SandboxTemplate) corev1.PullPolicy {
	if p := t.Spec.Image.PullPolicy; p != "" {
		return p
	}
	return corev1.PullIfNotPresent
}

// ImageRef is the reference a Pod actually runs.
//
// Templates are written with a tag; Pods run a digest. Everything between those
// two facts goes through here.
func ImageRef(t *sbxv1.SandboxTemplate) string {
	if d := t.Status.ResolvedDigest; d != "" {
		return pinned(t.Spec.Image.Ref, d)
	}
	return t.Spec.Image.Ref
}

func ptr[T any](v T) *T { return &v }

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PodToken reads back the credential a Pod was created with.
//
// The Pod spec is the only place it lives: nothing is stored, nothing is
// cached, and a controller that restarts simply reads it again.
func PodToken(p *corev1.Pod) string {
	for i := range p.Spec.Containers {
		for _, e := range p.Spec.Containers[i].Env {
			if e.Name == ControlTokenEnv {
				return e.Value
			}
		}
	}
	return ""
}

// NewControlToken mints a Pod's credential.
func NewControlToken() string {
	var b [24]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever did, an empty token
		// would silently disable authentication, so refuse instead.
		panic("jdix: cannot generate a control token: " + err.Error())
	}
	return "jct_" + hex.EncodeToString(b[:])
}
