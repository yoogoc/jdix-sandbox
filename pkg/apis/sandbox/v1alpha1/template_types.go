package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressMode is the default outbound posture for sandboxes from this template.
// +kubebuilder:validation:Enum=open;restricted;none
type EgressMode string

const (
	EgressOpen       EgressMode = "open"
	EgressRestricted EgressMode = "restricted"
	EgressNone       EgressMode = "none"
)

// ImageSpec pins the sandbox image.
type ImageSpec struct {
	// Ref is stored as a digest, not a tag. Admission resolves the tag once, at
	// template creation, so a warm pool can never end up holding two different
	// builds of the same "version".
	// +kubebuilder:validation:MinLength=1
	Ref           string                       `json:"ref"`
	PullSecretRef *corev1.LocalObjectReference `json:"pullSecretRef,omitempty"`
}

// TemplateVolume declares storage that sandboxes may mount subpaths of.
//
// It references a PVC that already exists in the tenant's namespace. There is
// no inline CSI configuration: several drivers let volumeAttributes name an
// arbitrary server and export path, which would hand a tenant the ability to
// mount anything at all.
type TemplateVolume struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
	// MountPath is where the volume appears inside the Pod, outside the sandbox
	// namespace. Sandboxes only ever see subpaths of it.
	MountPath string `json:"mountPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// NetworkSpec is the template's egress posture.
type NetworkSpec struct {
	// +kubebuilder:default=restricted
	Egress      EgressMode `json:"egress,omitempty"`
	AllowCIDRs  []string   `json:"allowCIDRs,omitempty"`
	DenyCIDRs   []string   `json:"denyCIDRs,omitempty"`
	AllowedFQDN []string   `json:"allowedFQDN,omitempty"`
}

// FilesystemDefaults are applied to sandboxes that do not override them.
type FilesystemDefaults struct {
	Workspace        Workspace `json:"workspace,omitempty"`
	AllowSystemPaths []string  `json:"allowSystemPaths,omitempty"`
}

// SandboxTemplateSpec is the blueprint.
type SandboxTemplateSpec struct {
	// MinIsolationTier is a floor, never a preference: a node that measures
	// below it is not used, even if that means a cold start or a failure.
	// Silently degrading an isolation guarantee is worse than not serving.
	// +kubebuilder:default=userns
	MinIsolationTier IsolationTier `json:"minIsolationTier,omitempty"`

	// RuntimeClassName is reserved for gVisor or Kata. Empty today; filling it
	// in later needs a node pool, not an architecture change.
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// +kubebuilder:default=1800
	DefaultTTLSeconds int32 `json:"defaultTTLSeconds,omitempty"`
	// +kubebuilder:default=14400
	MaxTTLSeconds int32 `json:"maxTTLSeconds,omitempty"`

	Image              ImageSpec          `json:"image"`
	FilesystemDefaults FilesystemDefaults `json:"filesystemDefaults,omitempty"`
	Volumes            []TemplateVolume   `json:"volumes,omitempty"`
	Network            NetworkSpec        `json:"network,omitempty"`

	// Resources is what one sandbox Pod requests and is limited to. Warm Pods
	// request the same: understating it to save money oversubscribes the node
	// and the tenant's workload is what gets OOM-killed.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// PodOverrides is merged into the generated Pod. Security-relevant fields
	// are re-applied by the controller afterwards, so this cannot be used to
	// weaken them.
	//
	// The schema is deliberately not expanded. Embedding a full PodSpec would
	// add roughly eight thousand lines to this CRD, which pushes `kubectl apply`
	// past its 256KB last-applied-configuration annotation and makes the CRD
	// itself unpleasant to install. Validation of what actually matters happens
	// in the controller, which has to re-apply the security fields regardless.
	//
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	PodOverrides *corev1.PodSpec `json:"podOverrides,omitempty"`
}

// AdmissionState records how far a template got through image and volume checks.
// +kubebuilder:validation:Enum=Pending;PendingReview;Approved;Rejected
type AdmissionState string

const (
	AdmissionPending       AdmissionState = "Pending"
	AdmissionPendingReview AdmissionState = "PendingReview"
	AdmissionApproved      AdmissionState = "Approved"
	AdmissionRejected      AdmissionState = "Rejected"
)

// SandboxTemplateStatus is the observed state.
type SandboxTemplateStatus struct {
	// Hash changes whenever anything that affects a Pod changes. Warm Pods carry
	// it as a label, which is how a rolling update knows what is stale.
	Hash string `json:"hash,omitempty"`

	Admission AdmissionState `json:"admission,omitempty"`
	// AdmissionReason is shown to the tenant verbatim, so it says what to fix.
	AdmissionReason string `json:"admissionReason,omitempty"`
	ResolvedDigest  string `json:"resolvedDigest,omitempty"`

	// HasVolumes drives review: a template with a volume implies a warm pool,
	// and a warm pool costs the platform money, so it needs a human.
	HasVolumes bool               `json:"hasVolumes,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxtpl
// +kubebuilder:printcolumn:name="Admission",type=string,JSONPath=`.status.admission`
// +kubebuilder:printcolumn:name="MinTier",type=string,JSONPath=`.spec.minIsolationTier`
// +kubebuilder:printcolumn:name="Hash",type=string,JSONPath=`.status.hash`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxTemplate is a reusable blueprint for sandboxes.
type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxTemplateSpec   `json:"spec,omitempty"`
	Status SandboxTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxTemplateList is a list of SandboxTemplate.
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxTemplate{}, &SandboxTemplateList{})
}
