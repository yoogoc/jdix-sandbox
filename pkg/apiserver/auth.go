package apiserver

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Principal is the authenticated caller.
type Principal struct {
	TenantID  string
	Namespace string
	KeyID     string
	Key       *APIKey
}

type principalKey struct{}

// PrincipalFrom returns the caller attached by Authenticate.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

// cacheTTL bounds how long a revoked key keeps working.
//
// Sixty seconds is a deliberate trade: verifying argon2id on every request
// would make the hash's cost the API's cost. Revocation is expected to take
// effect within the window, and the Console says so.
const cacheTTL = 60 * time.Second

type cacheEntry struct {
	key      *APIKey
	tenant   *Tenant
	cachedAt time.Time
}

// Authenticator resolves API keys, with a short cache in front of the store.
type Authenticator struct {
	Store Store
	Now   func() time.Time

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

func NewAuthenticator(s Store) *Authenticator {
	return &Authenticator{Store: s, cache: map[string]cacheEntry{}}
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Invalidate drops a key from the cache, for use when it is revoked.
func (a *Authenticator) Invalidate(keyID string) {
	a.mu.Lock()
	delete(a.cache, keyID)
	a.mu.Unlock()
}

func (a *Authenticator) lookup(ctx context.Context, keyID string) (*APIKey, *Tenant, error) {
	a.mu.RLock()
	e, ok := a.cache[keyID]
	a.mu.RUnlock()
	if ok && a.now().Sub(e.cachedAt) < cacheTTL {
		return e.key, e.tenant, nil
	}

	key, err := a.Store.APIKey(ctx, keyID)
	if err != nil {
		return nil, nil, err
	}
	tenant, err := a.Store.Tenant(ctx, key.TenantID)
	if err != nil {
		return nil, nil, err
	}
	a.mu.Lock()
	a.cache[keyID] = cacheEntry{key: key, tenant: tenant, cachedAt: a.now()}
	a.mu.Unlock()
	return key, tenant, nil
}

// Authenticate is the middleware. Every failure returns the same 401 with the
// same body: distinguishing "no such key" from "wrong secret" would turn the
// endpoint into an oracle for enumerating key ids.
func (a *Authenticator) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := bearerToken(r)
		if presented == "" {
			unauthorized(w)
			return
		}
		keyID, secret, err := ParseKey(presented)
		if err != nil {
			unauthorized(w)
			return
		}
		key, tenant, err := a.lookup(r.Context(), keyID)
		if err != nil {
			unauthorized(w)
			return
		}
		if !VerifySecret(secret, key.SecretHash) {
			unauthorized(w)
			return
		}
		if err := key.Valid(a.now()); err != nil {
			// Revoked and expired are worth distinguishing: the caller holds a
			// key that really was theirs, and a vague error sends them hunting
			// for a bug that is not there.
			writeErr(w, http.StatusUnauthorized, "key_"+strings.TrimPrefix(err.Error(), "API key "), err.Error())
			return
		}
		if !ipAllowed(key.IPAllowlist, clientIP(r)) {
			writeErr(w, http.StatusForbidden, "ip_not_allowed",
				"this API key may only be used from its allowlisted addresses")
			return
		}

		// Best effort: last-used is for the Console, never for a decision, so a
		// write failure must not fail the request.
		_ = a.Store.TouchAPIKey(r.Context(), keyID, a.now())

		p := &Principal{TenantID: tenant.ID, Namespace: tenant.Namespace, KeyID: keyID, Key: key}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// RequireScope guards a route.
func RequireScope(s Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok || !p.Key.Allows(s) {
			writeErr(w, http.StatusForbidden, "insufficient_scope",
				"this API key does not carry the "+string(s)+" scope")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	// Accepted for compatibility with clients that cannot set Authorization.
	return r.Header.Get("X-Jdix-Api-Key")
}

func clientIP(r *http.Request) string {
	// Only the last hop is trustworthy unless a proxy is explicitly configured,
	// so X-Forwarded-For is deliberately not consulted here: a caller can set it
	// to anything, and an IP allowlist that can be spoofed is worse than none.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func ipAllowed(allowlist []string, ip string) bool {
	if len(allowlist) == 0 {
		return true
	}
	addr := net.ParseIP(ip)
	for _, entry := range allowlist {
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && addr != nil && cidr.Contains(addr) {
				return true
			}
			continue
		}
		if entry == ip {
			return true
		}
	}
	return false
}

func unauthorized(w http.ResponseWriter) {
	writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key")
}
