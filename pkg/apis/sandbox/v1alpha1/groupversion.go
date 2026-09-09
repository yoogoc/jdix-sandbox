// Package v1alpha1 holds the jdix-sandbox custom resources.
//
// Three kinds, and no more: Sandbox is the unit of work, SandboxTemplate is the
// reusable blueprint, SandboxPool keeps warm Pods ready. Pods themselves are
// created directly rather than through a Deployment — see the reconciler for
// why an owning ReplicaSet would fight the bind.
//
// +kubebuilder:object:generate=true
// +groupName=sandbox.jdix.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version for these kinds.
	GroupVersion = schema.GroupVersion{Group: "sandbox.jdix.io", Version: "v1alpha1"}

	// SchemeBuilder registers the kinds with a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds these kinds to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
