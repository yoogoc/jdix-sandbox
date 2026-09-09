// Package api holds the wire types shared by the control plane, execd and the SDKs.
// These mirror the Sandbox CRD's spec.filesystem (see docs/DESIGN.md §02, §03.4).
package api

// FilesystemSpec is the tenant-supplied description of what the sandbox may see.
// Every field here is untrusted input: it arrives from the Sandbox CR, which the
// tenant writes. bwrap.Generate is the only thing allowed to turn it into mounts,
// and it validates every path first.
type FilesystemSpec struct {
	Workspace        Workspace `json:"workspace"`
	Mounts           []Mount   `json:"mounts,omitempty"`
	AllowSystemPaths []string  `json:"allowSystemPaths,omitempty"`
	Hide             []string  `json:"hide,omitempty"`
}

// Workspace is the sandbox's writable scratch directory.
type Workspace struct {
	Path      string `json:"path"`                // in-sandbox path, default /workspace
	SizeLimit string `json:"sizeLimit,omitempty"` // informational here; enforced by emptyDir sizeLimit
}

// Mount binds a subpath of a template-declared volume into the sandbox.
type Mount struct {
	Path     string      `json:"path"` // in-sandbox target
	Source   MountSource `json:"source"`
	ReadOnly bool        `json:"readOnly,omitempty"`
}

// MountSource names a volume declared by the SandboxTemplate. Tenants cannot
// name a host path or inline CSI parameters — see DESIGN.md §05.4 ④.
type MountSource struct {
	Volume  string `json:"volume"`
	SubPath string `json:"subPath,omitempty"`
}

// DefaultWorkspacePath is used when spec.workspace.path is empty.
const DefaultWorkspacePath = "/workspace"
