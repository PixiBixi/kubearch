package inspector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	authnk8s "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/sync/singleflight"
	"k8s.io/client-go/kubernetes"

	"github.com/PixiBixi/kubearch/internal/types"
)

// credTTL bounds how long resolved credentials are reused. Building them costs
// one ServiceAccount GET plus one GET per imagePullSecret, live against the API
// server. Paying that per image is what this cache exists to avoid. Pull
// secrets do rotate, hence the expiry (and the eviction on auth failure).
const credTTL = 5 * time.Minute

// PodAuth carries the authentication context derived from a pod spec.
type PodAuth struct {
	Namespace          string
	ServiceAccountName string
	ImagePullSecrets   []string
}

// cacheKey identifies the credentials a pod resolves to. Pods of the same
// workload, and usually of the same namespace, share one.
func (a PodAuth) cacheKey() string {
	secrets := slices.Sorted(slices.Values(a.ImagePullSecrets))
	return a.Namespace + "\x00" + a.ServiceAccountName + "\x00" + strings.Join(secrets, ",")
}

// cachedPuller is a registry client bound to one set of credentials. Reusing it
// also reuses the registry auth tokens it has already negotiated, which a
// fresh remote.Get would re-fetch on every image.
type cachedPuller struct {
	puller    *remote.Puller
	expiresAt time.Time
}

// Inspector fetches and parses OCI manifests from a container registry.
type Inspector struct {
	k8sClient kubernetes.Interface
	logger    *slog.Logger
	credTTL   time.Duration
	now       func() time.Time

	// group collapses concurrent misses on the same credentials into a single
	// API-server round-trip.
	group singleflight.Group

	mu      sync.Mutex
	pullers map[string]*cachedPuller
}

func New(k8sClient kubernetes.Interface, logger *slog.Logger) *Inspector {
	return &Inspector{
		k8sClient: k8sClient,
		logger:    logger,
		credTTL:   credTTL,
		now:       time.Now,
		pullers:   make(map[string]*cachedPuller),
	}
}

// Inspect returns the digest and list of supported platforms for imageRef.
// Auth is resolved via k8schain (imagePullSecrets + service account + anonymous fallback).
func (i *Inspector) Inspect(ctx context.Context, imageRef string, auth PodAuth) (digest string, platforms []types.Platform, err error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", nil, fmt.Errorf("parse reference %q: %w", imageRef, err)
	}

	puller, err := i.pullerFor(ctx, auth)
	if err != nil {
		return "", nil, err
	}

	desc, err := puller.Get(ctx, ref)
	if err != nil {
		if isAuthError(err) {
			// Credentials may have rotated under us; the next attempt rebuilds them.
			i.evict(auth)
		}
		return "", nil, fmt.Errorf("fetch manifest: %w", err)
	}

	digest = desc.Digest.String()

	// Multi-arch: OCI image index or Docker manifest list.
	// ImageIndex() errors on single-arch images (not a manifest list), which is expected.
	// A non-nil idxErr means either: (a) single-arch image, so fall through to the image
	// config path, or (b) transient parse error, which also falls through and produces a
	// single-platform result from the config. Log at debug to distinguish the two cases.
	idx, idxErr := desc.ImageIndex()
	if idxErr == nil {
		manifest, err := idx.IndexManifest()
		if err != nil {
			return "", nil, fmt.Errorf("parse index manifest: %w", err)
		}
		seen := make(map[types.Platform]struct{})
		for _, m := range manifest.Manifests {
			if m.Platform == nil || m.Platform.OS == "" || m.Platform.OS == "unknown" {
				continue // skip attestation blobs and malformed entries
			}
			p := types.Platform{OS: m.Platform.OS, Arch: m.Platform.Architecture}
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				platforms = append(platforms, p)
			}
		}
		return digest, platforms, nil
	}
	i.logger.Debug("ImageIndex() failed, treating as single-arch image", "image", imageRef, "err", idxErr)

	// Single-arch: read platform from image config.
	img, err := desc.Image()
	if err != nil {
		return "", nil, fmt.Errorf("get image: %w", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", nil, fmt.Errorf("get config file: %w", err)
	}
	return digest, []types.Platform{{OS: cf.OS, Arch: cf.Architecture}}, nil
}

// pullerFor returns a registry client for auth, building (and caching) one on
// a miss.
func (i *Inspector) pullerFor(ctx context.Context, auth PodAuth) (*remote.Puller, error) {
	key := auth.cacheKey()
	if p, ok := i.cached(key); ok {
		return p, nil
	}

	built, err, _ := i.group.Do(key, func() (any, error) {
		// Re-check: a concurrent caller may have filled the cache while we queued.
		if p, ok := i.cached(key); ok {
			return p, nil
		}

		kc, err := authnk8s.New(ctx, i.k8sClient, authnk8s.Options{
			Namespace:          auth.Namespace,
			ServiceAccountName: auth.ServiceAccountName,
			ImagePullSecrets:   auth.ImagePullSecrets,
		})
		if err != nil {
			return nil, fmt.Errorf("build keychain: %w", err)
		}
		p, err := remote.NewPuller(remote.WithAuthFromKeychain(kc))
		if err != nil {
			return nil, fmt.Errorf("build puller: %w", err)
		}
		i.store(key, p)
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	p, ok := built.(*remote.Puller)
	if !ok {
		return nil, fmt.Errorf("unexpected puller type %T", built)
	}
	return p, nil
}

func (i *Inspector) cached(key string) (*remote.Puller, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.pullers[key]
	if !ok || !i.now().Before(e.expiresAt) {
		return nil, false
	}
	return e.puller, true
}

func (i *Inspector) store(key string, p *remote.Puller) {
	now := i.now()

	i.mu.Lock()
	defer i.mu.Unlock()
	for k, e := range i.pullers {
		if !now.Before(e.expiresAt) {
			delete(i.pullers, k) // keep the map bounded by live credentials only
		}
	}
	i.pullers[key] = &cachedPuller{puller: p, expiresAt: now.Add(i.credTTL)}
}

func (i *Inspector) evict(auth PodAuth) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.pullers, auth.cacheKey())
}

// isAuthError reports whether the registry rejected our credentials, as opposed
// to the image being missing or the registry being unreachable.
func isAuthError(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	return terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden
}
