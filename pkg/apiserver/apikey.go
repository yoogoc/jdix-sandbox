// Package apiserver is the tenant-facing control plane: authentication,
// quotas, admission and the REST surface the SDKs speak.
package apiserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Key format: jdix_sk_<keyID>_<secret>
//
// The fixed prefix is not decoration. It lets logs be redacted by pattern, and
// it can be registered with secret scanners so a key pasted into a public repo
// is reported rather than quietly abused.
const (
	KeyPrefix    = "jdix_sk_"
	keyIDLen     = 12
	secretBytes  = 24
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
)

var (
	// ErrMalformedKey covers anything that is not shaped like one of our keys.
	// It is returned instead of a more specific error so a caller probing the
	// endpoint learns nothing about which part they got wrong.
	ErrMalformedKey = errors.New("malformed API key")
	ErrKeyNotFound  = errors.New("API key not found")
	ErrKeyRevoked   = errors.New("API key revoked")
	ErrKeyExpired   = errors.New("API key expired")
)

// GeneratedKey is returned once, at creation. The plaintext is never stored and
// cannot be recovered — which is the point, and which the UI has to say clearly.
type GeneratedKey struct {
	KeyID     string
	Plaintext string
	Hash      string
}

// GenerateKey mints a new API key.
func GenerateKey() (GeneratedKey, error) {
	idBytes := make([]byte, keyIDLen/2)
	if _, err := rand.Read(idBytes); err != nil {
		return GeneratedKey{}, err
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return GeneratedKey{}, err
	}
	keyID := hex.EncodeToString(idBytes)
	secretStr := base64.RawURLEncoding.EncodeToString(secret)

	hash, err := HashSecret(secretStr)
	if err != nil {
		return GeneratedKey{}, err
	}
	return GeneratedKey{
		KeyID:     keyID,
		Plaintext: KeyPrefix + keyID + "_" + secretStr,
		Hash:      hash,
	}, nil
}

// ParseKey splits a presented key into its id and secret without touching any
// storage, so an obviously malformed key costs no database round trip.
func ParseKey(presented string) (keyID, secret string, err error) {
	if !strings.HasPrefix(presented, KeyPrefix) {
		return "", "", ErrMalformedKey
	}
	rest := strings.TrimPrefix(presented, KeyPrefix)
	keyID, secret, ok := strings.Cut(rest, "_")
	if !ok || len(keyID) != keyIDLen || secret == "" {
		return "", "", ErrMalformedKey
	}
	return keyID, secret, nil
}

// HashSecret derives the stored verifier. argon2id is deliberate: an API key is
// high-entropy so a fast hash would arguably do, but the cost of being wrong
// about that is a stolen database becoming a stolen fleet of sandboxes.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(sum), nil
}

// VerifySecret checks a presented secret against a stored hash in constant time.
func VerifySecret(secret, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Scope is a capability an API key may carry.
type Scope string

const (
	ScopeSandboxCreate Scope = "sandbox:create"
	ScopeSandboxRead   Scope = "sandbox:read"
	ScopeSandboxDelete Scope = "sandbox:delete"
	ScopeTemplateRead  Scope = "template:read"
	ScopeTemplateWrite Scope = "template:write"
	ScopeAdmin         Scope = "admin:*"
)

// APIKey is the stored record. The plaintext secret is not part of it.
type APIKey struct {
	KeyID            string
	TenantID         string
	Name             string
	SecretHash       string
	Scopes           []Scope
	AllowedTemplates []string
	IPAllowlist      []string
	ExpiresAt        *time.Time
	RevokedAt        *time.Time
	LastUsedAt       *time.Time
	CreatedAt        time.Time
}

// Allows reports whether the key carries a scope.
func (k *APIKey) Allows(s Scope) bool {
	for _, have := range k.Scopes {
		if have == ScopeAdmin || have == s {
			return true
		}
	}
	return false
}

// AllowsTemplate reports whether the key may use a template. An empty allowlist
// means every template in the tenant, which is the common case.
func (k *APIKey) AllowsTemplate(name string) bool {
	if len(k.AllowedTemplates) == 0 {
		return true
	}
	for _, t := range k.AllowedTemplates {
		if t == name {
			return true
		}
	}
	return false
}

// Valid reports whether the key may be used at time now.
func (k *APIKey) Valid(now time.Time) error {
	if k.RevokedAt != nil {
		return ErrKeyRevoked
	}
	if k.ExpiresAt != nil && now.After(*k.ExpiresAt) {
		return ErrKeyExpired
	}
	return nil
}
