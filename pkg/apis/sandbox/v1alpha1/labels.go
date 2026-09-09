package v1alpha1

// Labels and annotations the controller puts on warm and bound Pods.
//
// The bind is a label swap under optimistic concurrency, so the state a Pod is
// in has to be expressible as labels — that is what makes a conflicting bind
// fail loudly instead of two Sandboxes sharing one Pod.
const (
	LabelPool          = "sandbox.jdix.io/pool"
	LabelTemplate      = "sandbox.jdix.io/template"
	LabelTemplateHash  = "sandbox.jdix.io/template-hash"
	LabelIsolationTier = "sandbox.jdix.io/isolation-tier"
	LabelState         = "sandbox.jdix.io/state"
	LabelSandboxID     = "sandbox.jdix.io/sandbox-id"
	LabelTenant        = "sandbox.jdix.io/tenant"
	LabelManagedBy     = "app.kubernetes.io/managed-by"

	AnnotationBoundAt   = "sandbox.jdix.io/bound-at"
	AnnotationExpiresAt = "sandbox.jdix.io/expires-at"
	AnnotationTierProbe = "sandbox.jdix.io/tier-probe"
	AnnotationMetadata  = "sandbox.jdix.io/metadata"

	// StateIdle is a warm Pod waiting to be claimed.
	StateIdle = "idle"
	// StateBound belongs to a Sandbox and will never be reused.
	StateBound = "bound"
	// StateDraining is on its way out; the pool must not count it.
	StateDraining = "draining"

	ManagedBy = "jdix-controller"

	// FinalizerSandbox guarantees the Pod is removed before the Sandbox object
	// disappears. Without it, deleting a Sandbox could orphan a running Pod —
	// the leak that fills a cluster fastest.
	FinalizerSandbox = "sandbox.jdix.io/cleanup"
)
