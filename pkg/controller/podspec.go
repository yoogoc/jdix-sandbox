// Package controller reconciles Sandbox, SandboxTemplate and SandboxPool.
package controller

import (
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

// TemplateHash fingerprints everything about a template that changes the Pod.
//
// Warm Pods carry it as a label, which is the whole mechanism behind rolling a
// pool: a Pod whose hash no longer matches the template is stale and gets
// replaced. Fields that do not affect the Pod are deliberately excluded, so
// editing a description does not churn the pool.
func TemplateHash(t *sbxv1.SandboxTemplate) string {
	material := struct {
		Image        sbxv1.ImageSpec             `json:"image"`
		MinTier      sbxv1.IsolationTier         `json:"minTier"`
		RuntimeClass string                      `json:"runtimeClass"`
		Volumes      []sbxv1.TemplateVolume      `json:"volumes"`
		Resources    corev1.ResourceRequirements `json:"resources"`
		Overrides    *corev1.PodSpec             `json:"overrides"`
		Defaults     sbxv1.FilesystemDefaults    `json:"defaults"`
	}{
		Image:        t.Spec.Image,
		MinTier:      t.Spec.MinIsolationTier,
		RuntimeClass: t.Spec.RuntimeClassName,
		Volumes:      t.Spec.Volumes,
		Resources:    t.Spec.Resources,
		Overrides:    t.Spec.PodOverrides,
		Defaults:     t.Spec.FilesystemDefaults,
	}
	b, err := json.Marshal(material)
	if err != nil {
		// Marshalling a struct of plain types cannot fail; if it somehow does,
		// a hash that changes every time is safer than one that never does.
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
func BuildPod(t *sbxv1.SandboxTemplate, pool string, platform PlatformImage, l bwrap.Layout, tokenSecret string) *corev1.Pod {
	hash := TemplateHash(t)
	nonRoot := true
	noEscalate := false
	uid := int64(1000)

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
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: []corev1.Container{{
			Name:            "jdix-installer",
			Image:           platform.Ref,
			ImagePullPolicy: platform.PullPolicy,
			Command:         []string{"/bin/sh", "-c"},
			Args: []string{
				// Copy rather than mount: the tenant image keeps its own /usr,
				// and the platform binaries live somewhere it cannot write.
				fmt.Sprintf("cp -a /opt/jdix/bin/. %s/ && mkdir -p %s %s %s && chmod 700 %s",
					bwrap.PlatformRoot+"/bin", l.WorkspaceDir, l.VolumeRoot, l.IPCDir, l.IPCDir),
			},
			VolumeMounts: platformMounts(l),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noEscalate,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Containers: []corev1.Container{{
			Name:            "sandbox",
			Image:           t.Spec.Image.Ref,
			ImagePullPolicy: corev1.PullIfNotPresent, // the ref is a digest, so caching is safe
			Command:         []string{path.Join(bwrap.PlatformRoot, "bin", "jdix-execd")},
			Args: []string{
				fmt.Sprintf("--data-addr=:%d", PortData),
				fmt.Sprintf("--control-addr=:%d", PortControl),
				"--bwrap=" + path.Join(bwrap.PlatformRoot, "bin", "bwrap"),
				"--volumes=" + VolumeArgs(l, t),
				"--internal-token-file=" + path.Join(bwrap.PlatformRoot, "secret", "control-token"),
			},
			Ports: []corev1.ContainerPort{
				{Name: "data", ContainerPort: PortData},
				{Name: "control", ContainerPort: PortControl},
			},
			Resources:    t.Spec.Resources,
			VolumeMounts: append(platformMounts(l), templateVolumeMounts(l, t)...),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noEscalate,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
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
		Volumes: podVolumes(l, t, tokenSecret),
	}

	if t.Spec.Image.PullSecretRef != nil {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{*t.Spec.Image.PullSecretRef}
	}
	if t.Spec.PodOverrides != nil {
		mergeOverrides(&spec, t.Spec.PodOverrides)
	}

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
		Spec: spec,
	}
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
	out = append(out, corev1.VolumeMount{
		Name:      "jdix-secret",
		MountPath: path.Join(bwrap.PlatformRoot, "secret"),
		ReadOnly:  true,
	})
	return out
}

func podVolumes(l bwrap.Layout, t *sbxv1.SandboxTemplate, tokenSecret string) []corev1.Volume {
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
		{Name: "jdix-secret", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: tokenSecret, DefaultMode: ptr(int32(0o400))},
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

func ptr[T any](v T) *T { return &v }

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
