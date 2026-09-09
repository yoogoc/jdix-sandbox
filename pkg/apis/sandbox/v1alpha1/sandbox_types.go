package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsolationTier is how strongly a node can separate the sandbox from the Pod.
// +kubebuilder:validation:Enum=userns;capadmin;chroot
type IsolationTier string

const (
	TierUserns   IsolationTier = "userns"
	TierCapAdmin IsolationTier = "capadmin"
	TierChroot   IsolationTier = "chroot"
)

// SandboxPhase tracks a Sandbox through its life.
// +kubebuilder:validation:Enum=Pending;Binding;Running;Expired;Failed;Terminating
type SandboxPhase string

const (
	PhasePending     SandboxPhase = "Pending"
	PhaseBinding     SandboxPhase = "Binding"
	PhaseRunning     SandboxPhase = "Running"
	PhaseExpired     SandboxPhase = "Expired"
	PhaseFailed      SandboxPhase = "Failed"
	PhaseTerminating SandboxPhase = "Terminating"
)

// Workspace is the sandbox's writable scratch directory.
type Workspace struct {
	// +kubebuilder:default="/workspace"
	Path string `json:"path,omitempty"`
	// SizeLimit is applied to the backing emptyDir.
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// MountSource names a volume the SandboxTemplate declared. There is no field
// for a host path or for inline CSI attributes, and that is the point: a tenant
// can only reach storage an administrator already approved.
type MountSource struct {
	// +kubebuilder:validation:MinLength=1
	Volume string `json:"volume"`
	// SubPath is resolved beneath the volume root and may not escape it.
	SubPath string `json:"subPath,omitempty"`
}

// Mount binds part of a declared volume into the sandbox.
type Mount struct {
	// +kubebuilder:validation:Pattern=`^/.*`
	Path     string      `json:"path"`
	Source   MountSource `json:"source"`
	ReadOnly bool        `json:"readOnly,omitempty"`
}

// FilesystemSpec is what the sandbox is allowed to see. It is the sole input to
// the bubblewrap argument generator, and it only exists once a sandbox is
// created — which is why the namespace cannot be built during warm-up.
type FilesystemSpec struct {
	Workspace Workspace `json:"workspace,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	Mounts []Mount `json:"mounts,omitempty"`
	// AllowSystemPaths must be a subset of the platform allowlist.
	AllowSystemPaths []string `json:"allowSystemPaths,omitempty"`
	// Hide blanks a path that would otherwise be visible.
	Hide []string `json:"hide,omitempty"`
}

// EnvVar is a plain environment entry. Anything sensitive belongs in SecretRefs.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// SecretRef names a Secret whose contents execd injects into the sandbox's
// environment in memory. Values never enter the Pod spec, so they do not appear
// in `kubectl get pod -o yaml`.
type SecretRef struct {
	Name string `json:"name"`
}

// SandboxSpec is the desired state of a Sandbox.
type SandboxSpec struct {
	// +kubebuilder:validation:MinLength=1
	TemplateRef string `json:"templateRef"`
	// +kubebuilder:validation:Minimum=1
	TTLSeconds int32          `json:"ttlSeconds,omitempty"`
	Filesystem FilesystemSpec `json:"filesystem,omitempty"`
	Env        []EnvVar       `json:"env,omitempty"`
	SecretRefs []SecretRef    `json:"secretRefs,omitempty"`
	// Metadata is opaque tenant bookkeeping, echoed back on reads.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SandboxStatus is the observed state.
type SandboxStatus struct {
	Phase    SandboxPhase `json:"phase,omitempty"`
	PodName  string       `json:"podName,omitempty"`
	PodIP    string       `json:"podIP,omitempty"`
	NodeName string       `json:"nodeName,omitempty"`

	IsolationTier IsolationTier `json:"isolationTier,omitempty"`
	// ColdStart records whether the warm pool was missed. It is the most direct
	// signal that a pool is undersized, so it is surfaced rather than logged.
	ColdStart bool   `json:"coldStart,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`

	BoundAt   *metav1.Time `json:"boundAt,omitempty"`
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Reason explains a Failed or Expired phase in words a tenant can act on.
	Reason     string             `json:"reason,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbx
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.templateRef`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.status.isolationTier`
// +kubebuilder:printcolumn:name="Cold",type=boolean,JSONPath=`.status.coldStart`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Sandbox is one isolated execution environment, backed by exactly one Pod for
// the whole of its life.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec,omitempty"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList is a list of Sandbox.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
