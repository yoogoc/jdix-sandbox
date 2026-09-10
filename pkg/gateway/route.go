// Package gateway routes public traffic to sandbox Pods.
//
// Two schemes are supported, and they fail differently.
//
// Host routing publishes each sandbox on its own subdomain,
// sbx-abc.sbx.example.com. Every sandbox is then a distinct origin, so the
// browser's same-origin policy separates one tenant's exposed web app from the
// next. It costs a wildcard certificate — issuable only over DNS-01 — and a
// wildcard DNS record.
//
// Path routing publishes them all under one host,
// api.example.com/s/sbx-abc/... One certificate, one DNS record, and the "-8000"
// ambiguity below disappears because the port becomes its own segment. It has a
// cost worth stating plainly rather than discovering: every sandbox shares a
// single origin, so the same-origin policy stops separating them entirely.
// Script served from one sandbox's exposed port can read another's. That is
// harmless for the SDK data plane, where no browser is involved, and it is the
// operator's decision to accept for user ports.
//
// The gateway therefore rewrites Location and Set-Cookie headers under path
// routing, and deliberately does not rewrite HTML, CSS or JavaScript. A proxy
// editing a response body can only guess — inline scripts concatenate URLs,
// CSS has url(), HTML has srcset, and a rewriter that handles four of those
// five convinces everyone it handles the fifth. Applications behind path
// routing are expected to support sub-path deployment; X-Forwarded-Prefix tells
// them where they live.
package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DataPlanePort is where execd serves the authenticated data plane.
const DataPlanePort = 8080

// DefaultPathPrefix is where path routing looks for sandbox traffic. It has to
// stay disjoint from the control plane's own /v1/ so that one hostname, one
// certificate and one Ingress can serve both.
const DefaultPathPrefix = "/s"

// userPortSegment separates a port the sandbox opened from the platform's own
// data plane. The data plane lives entirely under /v1/, so there is nothing for
// this to collide with.
const userPortSegment = "p"

// Route is what a request resolved to.
type Route struct {
	SandboxID string
	Port      int
	// UserPort is true when the caller asked for a port the sandbox opened,
	// rather than for the platform's own data plane.
	UserPort bool

	// Rest and RawRest are the path to hand upstream, with any routing prefix
	// removed. Both are kept because one decoded path cannot represent an
	// escaped separator: "%2F" and "/" decode alike and mean different things
	// to the sandbox.
	//
	// An empty Rest means the URL named the sandbox with no trailing slash. The
	// server redirects rather than forwarding it — see Server.route.
	Rest    string
	RawRest string

	// Prefix is what was stripped from the front, so a redirect or a Location
	// header can put it back. Empty under host routing, where nothing was.
	Prefix string
}

var errNoRoute = errors.New("request does not name a sandbox")

// Exists reports whether a sandbox id is known. Splitting a hostname is
// ambiguous on its own, so the resolver settles it.
type Exists func(id string) bool

// Router resolves a request to a sandbox, and renders the public URLs of one.
//
// The two directions are inverses, so one type owns both. A router that parsed
// one scheme and rendered another would hand out URLs its own gateway rejects,
// and nothing else in the system reads both halves closely enough to notice.
type Router interface {
	// Route resolves a request, or returns an error if it names no sandbox.
	Route(r *http.Request) (Route, error)
	// EndpointURL is the sandbox's data-plane base: what the controller writes
	// to status.endpoint and what the SDKs prefix every data-plane call with.
	EndpointURL(id string) string
	// PortURL is where a port opened inside the sandbox is published.
	//
	// Both may return a scheme-relative ("//host/path") or origin-relative
	// ("/path") reference when the router was not configured with a public
	// origin, which is the normal case inside the gateway: the request itself
	// says what origin it arrived on. Callers with no request — the controller —
	// configure Scheme or Base and get absolute URLs back.
	PortURL(id string, port int) string
}

// HostRouter publishes each sandbox on its own subdomain.
type HostRouter struct {
	// Suffix is the wildcard domain, e.g. "sbx.example.com".
	Suffix string
	// Scheme is used when rendering URLs. Empty renders scheme-relative.
	Scheme string
	// Resolver settles the "-8000" ambiguity described on ParseHost. Optional:
	// without it the port reading is taken at face value.
	Resolver Resolver
}

func (h HostRouter) Route(r *http.Request) (Route, error) {
	var exists Exists
	if h.Resolver != nil {
		exists = func(id string) bool {
			_, ok := h.Resolver.Resolve(r.Context(), id)
			return ok
		}
	}
	rt, err := ParseHost(r.Host, h.Suffix, exists)
	if err != nil {
		return Route{}, err
	}
	// The hostname carried the routing information, so the path is the
	// sandbox's own and travels untouched.
	rt.Rest, rt.RawRest = r.URL.Path, r.URL.EscapedPath()
	if rt.Rest == "" {
		rt.Rest, rt.RawRest = "/", "/"
	}
	return rt, nil
}

func (h HostRouter) EndpointURL(id string) string {
	return h.origin(id + "." + strings.TrimPrefix(h.Suffix, "."))
}

func (h HostRouter) PortURL(id string, port int) string {
	return h.origin(id+"-"+strconv.Itoa(port)+"."+strings.TrimPrefix(h.Suffix, ".")) + "/"
}

func (h HostRouter) origin(host string) string {
	if h.Scheme == "" {
		return "//" + host
	}
	return h.Scheme + "://" + host
}

// PathRouter publishes every sandbox under one hostname.
type PathRouter struct {
	// Prefix is the routing prefix, DefaultPathPrefix when empty.
	Prefix string
	// Base is the public origin URLs are rendered against, e.g.
	// "https://api.example.com". Routing never consults it; it exists because
	// the controller renders URLs with no request in hand.
	Base string
}

func (p PathRouter) prefix() string {
	if p.Prefix == "" {
		return DefaultPathPrefix
	}
	return "/" + strings.Trim(p.Prefix, "/")
}

func (p PathRouter) Route(r *http.Request) (Route, error) {
	prefix := p.prefix()
	esc := r.URL.EscapedPath()
	if esc != prefix && !strings.HasPrefix(esc, prefix+"/") {
		return Route{}, errNoRoute
	}

	// Segments are cut from the escaped path, not the decoded one, so that a
	// sandbox id can never be forged with %2F.
	id, rest := cutSegment(strings.TrimPrefix(esc, prefix))
	if id == "" {
		return Route{}, errNoRoute
	}
	rt := Route{SandboxID: id, Port: DataPlanePort, Prefix: prefix + "/" + id}

	if head, after := cutSegment(rest); head == userPortSegment {
		portSeg, tail := cutSegment(after)
		port, err := strconv.Atoi(portSeg)
		if err != nil || port <= 0 || port > 65535 {
			return Route{}, errNoRoute
		}
		rt.Port, rt.UserPort = port, true
		rt.Prefix += "/" + userPortSegment + "/" + portSeg
		rest = tail
	}

	dec, err := url.PathUnescape(rest)
	if err != nil {
		return Route{}, errNoRoute
	}
	rt.Rest, rt.RawRest = dec, rest
	return rt, nil
}

func (p PathRouter) EndpointURL(id string) string {
	return strings.TrimSuffix(p.Base, "/") + p.prefix() + "/" + id
}

func (p PathRouter) PortURL(id string, port int) string {
	return p.EndpointURL(id) + "/" + userPortSegment + "/" + strconv.Itoa(port) + "/"
}

// ChainRouter accepts every scheme in it, so one gateway can serve both during
// a migration. URLs are rendered by the first entry: accepting an old scheme
// and handing out new URLs is exactly what migrating consists of.
type ChainRouter []Router

func (c ChainRouter) Route(r *http.Request) (Route, error) {
	for _, router := range c {
		if rt, err := router.Route(r); err == nil {
			return rt, nil
		}
	}
	return Route{}, errNoRoute
}

func (c ChainRouter) EndpointURL(id string) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].EndpointURL(id)
}

func (c ChainRouter) PortURL(id string, port int) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].PortURL(id, port)
}

// cutSegment splits "/a/b/c" into "a" and "/b/c", and "/a" into "a" and "".
// An input not beginning with a slash yields no segment.
func cutSegment(p string) (seg, rest string) {
	if !strings.HasPrefix(p, "/") {
		return "", p
	}
	p = p[1:]
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i:]
	}
	return p, ""
}

// ParseHost turns a Host header into a route.
//
//	sbx-abc123.sbx.example.com        -> the data plane
//	sbx-abc123-8000.sbx.example.com   -> port 8000 inside the sandbox
//
// Sandbox ids contain a hyphen themselves, so "-8000" cannot be told from part
// of an id by inspection alone. Rather than constrain the id format — a rule
// someone would eventually break — the ambiguity is resolved by asking whether
// the candidate sandbox actually exists. Path routing has no such problem.
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
