package inspector

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/client-go/kubernetes"

	"github.com/PixiBixi/kubearch/internal/store"
)

// PodAuth carries the authentication context derived from a pod spec.
type PodAuth struct {
	Namespace          string
	ServiceAccountName string
	ImagePullSecrets   []string
}

// Inspector fetches and parses OCI manifests from a container registry.
type Inspector struct {
	k8sClient kubernetes.Interface
}

func New(k8sClient kubernetes.Interface) *Inspector {
	return &Inspector{k8sClient: k8sClient}
}

// Inspect returns the digest and list of supported platforms for imageRef.
// Auth is resolved via k8schain (imagePullSecrets + service account + anonymous fallback).
func (i *Inspector) Inspect(ctx context.Context, imageRef string, auth PodAuth) (digest string, platforms []store.Platform, err error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", nil, fmt.Errorf("parse reference %q: %w", imageRef, err)
	}

	kc, err := k8schain.New(ctx, i.k8sClient, k8schain.Options{
		Namespace:          auth.Namespace,
		ServiceAccountName: auth.ServiceAccountName,
		ImagePullSecrets:   auth.ImagePullSecrets,
	})
	if err != nil {
		return "", nil, fmt.Errorf("build keychain: %w", err)
	}

	desc, err := remote.Get(ref,
		remote.WithAuthFromKeychain(kc),
		remote.WithContext(ctx),
	)
	if err != nil {
		return "", nil, fmt.Errorf("fetch manifest: %w", err)
	}

	digest = desc.Digest.String()

	// Multi-arch: OCI image index or Docker manifest list.
	if idx, idxErr := desc.ImageIndex(); idxErr == nil {
		manifest, err := idx.IndexManifest()
		if err != nil {
			return "", nil, fmt.Errorf("parse index manifest: %w", err)
		}
		seen := make(map[store.Platform]bool)
		for _, m := range manifest.Manifests {
			if m.Platform == nil || m.Platform.OS == "" || m.Platform.OS == "unknown" {
				continue // skip attestation blobs and malformed entries
			}
			p := store.Platform{OS: m.Platform.OS, Arch: m.Platform.Architecture}
			if !seen[p] {
				seen[p] = true
				platforms = append(platforms, p)
			}
		}
		return digest, platforms, nil
	}

	// Single-arch: read platform from image config.
	img, err := desc.Image()
	if err != nil {
		return "", nil, fmt.Errorf("get image: %w", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", nil, fmt.Errorf("get config file: %w", err)
	}
	return digest, []store.Platform{{OS: cf.OS, Arch: cf.Architecture}}, nil
}
