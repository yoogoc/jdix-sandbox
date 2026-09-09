package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ResolvedImage is what a tag turned out to mean, at one moment in time.
type ResolvedImage struct {
	Digest string // sha256:...
	Bytes  int64  // compressed size of the manifest's layers
}

// ImageResolver turns a reference a human wrote into a digest the platform can
// pin.
//
// This exists so a template can say `registry/py312:2026-09-01` — which is what
// anyone would type — while everything downstream works from an immutable
// digest. Making the user find the digest themselves is not a simplification;
// it just moves the work, and to the one place it cannot be done, since the
// digest only exists after a push.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string, pullSecrets []string, namespace string) (ResolvedImage, error)
}

// ErrImageNotFound means the reference does not exist, or the credentials
// cannot see it. It is separated from transport failures because one is the
// tenant's problem and the other is ours.
var ErrImageNotFound = errors.New("image not found")

// RegistryResolver talks to the registry.
type RegistryResolver struct {
	Keychain authn.Keychain
	Client   kubernetes.Interface
	Timeout  time.Duration
}

// NewRegistryResolver builds a resolver that authenticates with the same pull
// secrets the Pod would use, so a template resolves exactly the image its Pods
// will run — not whatever an anonymous request happens to see.
func NewRegistryResolver(cfg *rest.Config) (*RegistryResolver, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &RegistryResolver{Client: cs, Timeout: 30 * time.Second}, nil
}

func (r *RegistryResolver) keychain(ctx context.Context, namespace string, secrets []string) (authn.Keychain, error) {
	if r.Keychain != nil {
		return r.Keychain, nil
	}
	if r.Client == nil || len(secrets) == 0 {
		// Anonymous is right for a public registry and hopeless for a private
		// one; the caller finds out through a not-found error either way.
		return authn.DefaultKeychain, nil
	}
	kc := dockerConfigKeychain{auths: map[string]authn.AuthConfig{}}
	for _, name := range secrets {
		sec, err := r.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// One unreadable secret should not blind the resolver to the others,
			// and an anonymous attempt still tells us something useful.
			continue
		}
		kc.add(sec)
	}
	if len(kc.auths) == 0 {
		return authn.DefaultKeychain, nil
	}
	return kc, nil
}

// dockerConfigKeychain resolves registry credentials from imagePullSecrets.
//
// go-containerregistry ships a Kubernetes keychain, but it lives in a separate
// module that drags in a second copy of the Kubernetes client. Reading a
// dockerconfigjson Secret is a dozen lines, so the dependency is not worth it.
type dockerConfigKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k dockerConfigKeychain) add(sec *corev1.Secret) {
	raw, ok := sec.Data[corev1.DockerConfigJsonKey]
	if !ok {
		// The legacy .dockercfg format is a bare map of host -> auth.
		if legacy, ok := sec.Data[corev1.DockerConfigKey]; ok {
			var entries map[string]dockerAuthEntry
			if json.Unmarshal(legacy, &entries) == nil {
				for host, e := range entries {
					k.auths[registryKey(host)] = e.toAuthConfig()
				}
			}
		}
		return
	}
	var cfg struct {
		Auths map[string]dockerAuthEntry `json:"auths"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return
	}
	for host, e := range cfg.Auths {
		k.auths[registryKey(host)] = e.toAuthConfig()
	}
}

type dockerAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func (e dockerAuthEntry) toAuthConfig() authn.AuthConfig {
	if e.Username == "" && e.Auth != "" {
		// "auth" is base64("user:pass"); the split is on the first colon,
		// because passwords may contain one.
		if decoded, err := base64.StdEncoding.DecodeString(e.Auth); err == nil {
			if u, p, ok := strings.Cut(string(decoded), ":"); ok {
				e.Username, e.Password = u, p
			}
		}
	}
	return authn.AuthConfig{Username: e.Username, Password: e.Password}
}

// registryKey normalises the many spellings of a registry host that appear in
// a docker config: with a scheme, with a path, and Docker Hub's several names.
func registryKey(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "index.docker.io", "docker.io", "registry-1.docker.io":
		return name.DefaultRegistry
	}
	return host
}

func (k dockerConfigKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if cfg, ok := k.auths[registryKey(res.RegistryStr())]; ok {
		return authn.FromConfig(cfg), nil
	}
	return authn.Anonymous, nil
}

func (r *RegistryResolver) Resolve(ctx context.Context, ref string, pullSecrets []string, namespace string) (ResolvedImage, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ResolvedImage{}, fmt.Errorf("%q is not a valid image reference: %w", ref, err)
	}
	kc, err := r.keychain(ctx, namespace, pullSecrets)
	if err != nil {
		return ResolvedImage{}, fmt.Errorf("reading pull secrets: %w", err)
	}

	desc, err := remote.Get(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(kc))
	if err != nil {
		if isNotFound(err) {
			return ResolvedImage{}, fmt.Errorf("%w: %s", ErrImageNotFound, ref)
		}
		return ResolvedImage{}, fmt.Errorf("resolving %s: %w", ref, err)
	}

	out := ResolvedImage{Digest: desc.Digest.String()}
	// A manifest list has no single size; the per-platform image does. Summing
	// the layers of the image we would actually pull is the number that matters
	// for the size limit and for how long a cold start takes.
	if img, err := desc.Image(); err == nil {
		if layers, err := img.Layers(); err == nil {
			for _, l := range layers {
				if sz, err := l.Size(); err == nil {
					out.Bytes += sz
				}
			}
		}
	}
	return out, nil
}

func isNotFound(err error) bool {
	// 401 and 403 are folded in with 404 on purpose: to a tenant they mean the
	// same thing — the platform cannot see that image with the credentials it
	// was given — and the fix is the same either way.
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode == 404 || te.StatusCode == 401 || te.StatusCode == 403
	}
	s := err.Error()
	return strings.Contains(s, "MANIFEST_UNKNOWN") ||
		strings.Contains(s, "NAME_UNKNOWN") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "unauthorized")
}

// splitRef reports the digest embedded in a reference, if any.
//
// A reference that already carries one needs no resolution — which is also the
// escape hatch for a locally built image that was never pushed anywhere.
func splitRef(ref string) (repo, digest string) {
	if i := strings.Index(ref, "@sha256:"); i >= 0 {
		d := ref[i+1:]
		if len(d) == len("sha256:")+64 {
			return ref[:i], d
		}
	}
	return ref, ""
}

// pinned rewrites a reference to name@digest.
func pinned(ref, digest string) string {
	repo, existing := splitRef(ref)
	if existing != "" {
		return ref
	}
	// Drop the tag: a reference may carry both, but the digest is what decides.
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + "@" + digest
}
