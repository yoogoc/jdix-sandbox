// Package gateway routes public traffic to sandbox Pods.
//
// Sandboxes are published on subdomains rather than path prefixes. A path
// prefix rewrites nothing inside the sandbox, so any web application serving
// absolute URLs — which is most of them — breaks the moment it emits its first
// "/static/app.js". A subdomain per sandbox costs one wildcard certificate and
// makes the problem disappear.
package gateway

import (
	"errors"
	"strconv"
	"strings"
)

// DataPlanePort is where execd serves the authenticated data plane.
const DataPlanePort = 8080

// Route is what a Host header resolved to.
type Route struct {
	SandboxID string
	Port      int
	// UserPort is true when the caller asked for a port the sandbox opened,
	// rather than for the platform's own data plane.
	UserPort bool
}

var errNoRoute = errors.New("host does not name a sandbox")

// Exists reports whether a sandbox id is known. Splitting the host is ambiguous
// on its own, so the resolver settles it.
type Exists func(id string) bool

// ParseHost turns a Host header into a route.
//
//	sbx-abc123.sbx.example.com        -> the data plane
//	sbx-abc123-8000.sbx.example.com   -> port 8000 inside the sandbox
//
// Sandbox ids contain a hyphen themselves, so "-8000" cannot be told from part
// of an id by inspection alone. Rather than constrain the id format — a rule
// someone would eventually break — the ambiguity is resolved by asking whether
// the candidate sandbox actually exists.
func ParseHost(host, suffix string, exists Exists) (Route, error) {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix = strings.ToLower(strings.TrimPrefix(suffix, "."))

	if !strings.HasSuffix(host, "."+suffix) {
		return Route{}, errNoRoute
	}
	label := strings.TrimSuffix(host, "."+suffix)
	if label == "" || strings.Contains(label, ".") {
		return Route{}, errNoRoute
	}

	// Prefer the port reading when it is plausible and the sandbox exists.
	if i := strings.LastIndexByte(label, '-'); i > 0 {
		if port, err := strconv.Atoi(label[i+1:]); err == nil && port > 0 && port < 65536 {
			id := label[:i]
			if exists == nil || exists(id) {
				return Route{SandboxID: id, Port: port, UserPort: true}, nil
			}
		}
	}
	if exists != nil && !exists(label) {
		return Route{}, errNoRoute
	}
	return Route{SandboxID: label, Port: DataPlanePort}, nil
}
