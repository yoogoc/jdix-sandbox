package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoscaleSpec sizes the pool from recent demand.
type AutoscaleSpec struct {
	// Enabled is off by default: a fixed pool is predictable, and predictable is
	// what a cost budget needs first.
	Enabled bool `json:"enabled,omitempty"`
	// TargetIdleSeconds is how much future demand the idle pool should cover.
	// +kubebuilder:default=60
	TargetIdleSeconds int32 `json:"targetIdleSeconds,omitempty"`
}

// SandboxPoolSpec keeps warm Pods ready for a template.
//
// Replicas is written by platform administrators, never by tenants: the
// platform pays for warm capacity, so allowing a tenant to set it would let any
// one of them drain the budget.
type SandboxPoolSpec struct {
	// +kubebuilder:validation:MinLength=1
	TemplateRef string `json:"templateRef"`

	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
	// +kubebuilder:validation:Minimum=0
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=0
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	Autoscale AutoscaleSpec `json:"autoscale,omitempty"`

	// NotReadyTimeoutSeconds is how long a warm Pod may take to become ready
	// before it is treated as stuck.
	//
	// Templates with volumes need a generous value. A CSI attach can take tens
	// of seconds, and a timeout shorter than the attach deletes Pods that were
	// about to succeed — which produces a pool that rebuilds forever and never
	// fills. This is the single easiest way to break a warm pool.
	// +kubebuilder:default=300
	NotReadyTimeoutSeconds int32 `json:"notReadyTimeoutSeconds,omitempty"`

	// IdleTTLSeconds recycles Pods that have been waiting too long, so a pool
	// does not quietly go on serving a months-old image layer.
	// +kubebuilder:default=3600
	IdleTTLSeconds int32 `json:"idleTTLSeconds,omitempty"`

	// MaxSurge bounds how many Pods a template rollout replaces at once.
	// +kubebuilder:default=3
	MaxSurge int32 `json:"maxSurge,omitempty"`
}

// SandboxPoolStatus is the observed state.
type SandboxPoolStatus struct {
	// IdleByTier is the pool's real currency: a Pod only counts for a template
	// whose minIsolationTier its node can actually meet.
	IdleByTier map[string]int32 `json:"idleByTier,omitempty"`

	Idle  int32 `json:"idle"`
	Bound int32 `json:"bound"`
	// Stale is warm Pods running a superseded template hash.
	Stale int32 `json:"stale,omitempty"`
	// NotReady is warm Pods still starting; a number that stays high usually
	// means an attach or pull problem, not a busy cluster.
	NotReady int32 `json:"notReady,omitempty"`

	TemplateHash string             `json:"templateHash,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxpool
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.templateRef`
// +kubebuilder:printcolumn:name="Want",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Idle",type=integer,JSONPath=`.status.idle`
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.bound`
// +kubebuilder:printcolumn:name="NotReady",type=integer,JSONPath=`.status.notReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxPool maintains warm Pods for a template.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList is a list of SandboxPool.
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
