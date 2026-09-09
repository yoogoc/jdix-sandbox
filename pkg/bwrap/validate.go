package bwrap

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"jdix.io/sandbox/pkg/api"
)

// ValidationError names the offending field so the API can hand the tenant an
// error they can act on, instead of a generic 400.
type ValidationError struct {
	Field  string
	Value  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s %q: %s", e.Field, e.Value, e.Reason)
}

func verr(field, value, reason string) error {
	return &ValidationError{Field: field, Value: value, Reason: reason}
}

// isAbsClean reports whether p is an absolute path already in canonical form
// with no traversal segments. We require the caller's literal string to be
// canonical rather than silently canonicalising it: a spec that says "/a/../b"
// is either a mistake or an attempt, and both deserve an error.
func isAbsClean(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.Contains(p, "\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	return path.Clean(p) == p
}

// overlaps reports whether a and b name the same path or one contains the other.
func overlaps(a, b string) bool { return contains(a, b) || contains(b, a) }

// contains reports whether parent is equal to child or is an ancestor of it.
func contains(parent, child string) bool {
	if parent == child {
		return true
	}
	if parent == "/" {
		return true
	}
	return strings.HasPrefix(child, parent+"/")
}

// protectedTargets are paths a tenant mount may never touch in either
// direction: mounting on top of them hides platform state, and mounting an
// ancestor of them shadows the whole subtree.
func (p Policy) protectedTargets(l Layout) []string {
	return []string{
		"/proc", "/dev",
		PlatformRoot, StateRoot,
		l.IPCMount, l.InitTarget,
		"/etc/passwd", "/etc/group", "/etc/resolv.conf",
	}
}

// shadowable paths may be mounted *inside* but never replaced wholesale. The
// sandbox is single-tenant, so a tenant shadowing part of /usr only affects
// itself; replacing /usr entirely would break the runtime in confusing ways.
func (p Policy) shadowable(spec api.FilesystemSpec) []string {
	out := append([]string{}, p.SystemAllowlist...)
	out = append(out, workspacePath(spec))
	return out
}

func workspacePath(spec api.FilesystemSpec) string {
	if spec.Workspace.Path == "" {
		return api.DefaultWorkspacePath
	}
	return spec.Workspace.Path
}

// Validate checks an untrusted FilesystemSpec against the platform policy.
// Generate calls it; call it directly at admission time so the tenant learns
// about a bad spec when they submit it, not when a sandbox fails to start.
func (p Policy) Validate(spec api.FilesystemSpec, l Layout) error {
	if p.Tier == TierChroot && len(spec.Mounts) > 0 {
		return verr("filesystem.mounts", "",
			"isolation tier 'chroot' has no mount namespace; spec.mounts is unsupported on this node (DESIGN.md §04.4)")
	}

	ws := workspacePath(spec)
	if !isAbsClean(ws) {
		return verr("filesystem.workspace.path", ws, "must be an absolute, canonical path with no '.' or '..' segments")
	}
	if ws == "/" {
		return verr("filesystem.workspace.path", ws, "must not be the root directory")
	}
	for _, prot := range p.protectedTargets(l) {
		if overlaps(ws, prot) {
			return verr("filesystem.workspace.path", ws, "overlaps platform path "+prot)
		}
	}

	max := p.MaxMounts
	if max <= 0 {
		max = MaxMounts
	}
	if len(spec.Mounts) > max {
		return verr("filesystem.mounts", fmt.Sprint(len(spec.Mounts)),
			fmt.Sprintf("at most %d mounts are allowed", max))
	}

	allowed := map[string]bool{}
	for _, s := range p.SystemAllowlist {
		allowed[s] = true
	}
	for _, sp := range spec.AllowSystemPaths {
		if !isAbsClean(sp) {
			return verr("filesystem.allowSystemPaths", sp, "must be an absolute, canonical path")
		}
		if !allowed[sp] {
			return verr("filesystem.allowSystemPaths", sp,
				"not in the platform allowlist ("+strings.Join(sortedKeys(allowed), ", ")+")")
		}
	}

	seen := map[string]bool{}
	shadowable := p.shadowable(spec)
	for i, m := range spec.Mounts {
		field := fmt.Sprintf("filesystem.mounts[%d].path", i)
		if !isAbsClean(m.Path) {
			return verr(field, m.Path, "must be an absolute, canonical path with no '.' or '..' segments")
		}
		if m.Path == "/" {
			return verr(field, m.Path, "must not be the root directory")
		}
		if seen[m.Path] {
			return verr(field, m.Path, "duplicate mount target")
		}
		seen[m.Path] = true

		for _, prot := range p.protectedTargets(l) {
			if overlaps(m.Path, prot) {
				return verr(field, m.Path, "overlaps platform path "+prot)
			}
		}
		// May mount inside a system path or the workspace, may not replace one.
		for _, sh := range shadowable {
			if contains(m.Path, sh) {
				return verr(field, m.Path, "would replace "+sh+"; mount inside it instead")
			}
		}

		sf := fmt.Sprintf("filesystem.mounts[%d].source", i)
		if m.Source.Volume == "" {
			return verr(sf+".volume", "", "must name a volume declared by the SandboxTemplate")
		}
		if _, ok := p.Volumes[m.Source.Volume]; !ok {
			return verr(sf+".volume", m.Source.Volume,
				"not declared by the SandboxTemplate; templates may only mount volumes they declare")
		}
		if err := validateSubPath(sf+".subPath", m.Source.SubPath); err != nil {
			return err
		}
	}

	for i, h := range spec.Hide {
		field := fmt.Sprintf("filesystem.hide[%d]", i)
		if !isAbsClean(h) {
			return verr(field, h, "must be an absolute, canonical path")
		}
		// Hides are applied after mounts, so a hide at or above a mount target
		// would silently blank the mount the tenant just asked for. Hiding a
		// path *inside* a mount is meaningful and stays allowed.
		for j, m := range spec.Mounts {
			if contains(h, m.Path) {
				return verr(field, h, fmt.Sprintf("would hide filesystem.mounts[%d].path %q", j, m.Path))
			}
		}
		if contains(h, ws) {
			return verr(field, h, "would hide the workspace at "+ws)
		}
		if h == "/" {
			return verr(field, h, "must not be the root directory")
		}
		for _, prot := range p.protectedTargets(l) {
			if overlaps(h, prot) {
				return verr(field, h, "overlaps platform path "+prot)
			}
		}
		for _, sh := range shadowable {
			if contains(h, sh) {
				return verr(field, h, "would hide all of "+sh)
			}
		}
	}
	return nil
}

// validateSubPath rejects anything that could escape the volume root. The
// lexical check here is necessary but not sufficient — a symlink inside the
// volume can still point out of it — so execd re-resolves the path with
// safepath.ResolveBeneath before handing it to bwrap.
func validateSubPath(field, sub string) error {
	if sub == "" {
		return nil
	}
	if strings.HasPrefix(sub, "/") {
		return verr(field, sub, "must be relative to the volume root")
	}
	if strings.Contains(sub, "\x00") {
		return verr(field, sub, "must not contain NUL")
	}
	for _, seg := range strings.Split(sub, "/") {
		if seg == ".." {
			return verr(field, sub, "must not contain '..' segments")
		}
	}
	if path.Clean(sub) != sub {
		return verr(field, sub, "must be in canonical form")
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
