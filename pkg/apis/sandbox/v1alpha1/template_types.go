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

// ImageSpec names the sandbox image.
type ImageSpec struct {
	// Ref may be written either way: name:tag, which is what people write, or
	// name@sha256:… A tag is resolved to a digest once at admission and the
	// result recorded in status.resolvedDigest; everything downstream uses the
	// digest, so repointing the tag afterwards cannot leave one warm pool
	// serving two different builds.
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`

	// Resolve turns tag resolution off for this template.
	//
	// Set it to false when the platform cannot reach the registry but the nodes
	// can, when the image was built locally and never pushed, or in an
	// air-gapped install. The kubelet then pulls by whatever Ref says, when a
	// Pod starts.
	//
	// The cost is real and worth stating: without a digest, two Pods in the same
	// warm pool can run different builds — a node that already cached the tag
	// keeps what it has while a fresh node pulls whatever the tag points at now.
	// The template hash also stops tracking the image, so moving the tag no
	// longer rolls the pool, and a misspelled reference is not caught at
	// admission: it surfaces later as every warm Pod failing to start, which
	// reads as "the pool never fills" rather than "the template is wrong".
	// +kubebuilder:default=true
	Resolve *bool `json:"resolve,omitempty"`

	// PullPolicy overrides how the kubelet fetches the image. It defaults to
	// IfNotPresent, which is right for a digest and is also what makes a
	// locally imported image usable; set Never for an image that exists only on
	// the node, or Always to trade warm-Pod start-up time for freshness.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

	PullSecretRef *corev1.LocalObjectReference `json:"pullSecretRef,omitempty"`
}

// ShouldResolve reports whether the tag should be resolved to a digest.
func (i ImageSpec) ShouldResolve() bool {
	return i.Resolve == nil || *i.Resolve
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

	// DefaultTTLSeconds applies when a sandbox names no lifetime of its own.
	// Negative makes this template's sandboxes permanent by default.
	// +kubebuilder:default=1800
	// +kubebuilder:validation:Minimum=-1
	DefaultTTLSeconds int32 `json:"defaultTTLSeconds,omitempty"`
	// MaxTTLSeconds caps every sandbox from this template, and is therefore
	// what decides whether this template's sandboxes may outlive their caller:
	// a template that caps lifetimes caps the permanent ones to the cap too.
	//
	// Zero or negative means no cap. Negative is accepted because the two
	// fields above read -1 as "no limit", and a neighbouring field where the
	// same value is a validation error is a trap rather than a distinction.
	// +kubebuilder:default=14400
	// +kubebuilder:validation:Minimum=-1
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

	// ResolvedDigest is what spec.image.ref meant at the moment it was resolved.
	//
	// A template may be written with a tag, which is what anyone would type.
	// The tag is resolved once and the digest recorded here; everything
	// downstream — the Pod, the pool's template hash — uses the digest. That is
	// what stops a tag being repointed later from leaving one warm pool serving
	// two different builds.
	ResolvedDigest string `json:"resolvedDigest,omitempty"`
	// ResolvedRef is the spec.image.ref this digest came from, so a changed ref
	// is re-resolved and an unchanged one is not.
	ResolvedRef string       `json:"resolvedRef,omitempty"`
	ResolvedAt  *metav1.Time `json:"resolvedAt,omitempty"`
	// ImageBytes is the compressed size of the image, which is mostly what a
	// cold start spends its time on. Zero when the image was never resolved.
	ImageBytes int64 `json:"imageBytes,omitempty"`
	// ImagePinned says whether Pods run an immutable reference. It is false for
	// a template that opted out of resolution, and is the answer to "why is this
	// pool running two different builds".
	ImagePinned bool `json:"imagePinned,omitempty"`

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
// +kubebuilder:printcolumn:name="Pinned",type=boolean,JSONPath=`.status.imagePinned`
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
