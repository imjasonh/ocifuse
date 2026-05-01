// Package oci wraps go-containerregistry to resolve an OCI reference into a
// single-platform image plus the authenticated HTTP transport needed to issue
// Range requests against its layer blobs.
package oci

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/imjasonh/ocifuse/internal/cache"
)

// SetCache enables a disk-backed cache for digest-keyed manifest and config
// fetches. When enabled, Resolve and ResolveDigest both honor the cache.
// Idempotent and safe to call before any Resolve.
var diskCache *cache.Cache

func SetCache(c *cache.Cache) { diskCache = c }

// DefaultPlatform is used when the caller does not override and the ref is a
// multi-arch index.
var DefaultPlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Image is a resolved single-platform OCI image plus enough context to issue
// authenticated HTTP Range requests against its layer blobs.
type Image struct {
	Ref       name.Reference
	Digest    v1.Hash
	Image     v1.Image
	Layers    []v1.Layer
	Transport http.RoundTripper
}

// Resolve fetches the manifest for ref (selecting the matching platform if
// the ref is an index) and returns a handle exposing the layer list plus an
// authenticated HTTP transport scoped to the source registry.
func Resolve(ctx context.Context, ref name.Reference, platform v1.Platform) (*Image, error) {
	tr, err := buildTransport(ctx, ref)
	if err != nil {
		return nil, err
	}

	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithTransport(tr),
		remote.WithPlatform(platform),
	)
	if err != nil {
		return nil, fmt.Errorf("fetch image %s: %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("image digest: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("image layers: %w", err)
	}

	return &Image{
		Ref:       ref,
		Digest:    digest,
		Image:     img,
		Layers:    layers,
		Transport: tr,
	}, nil
}

// BlobURL constructs the registry blob URL for content digest d in this
// image's repository. Suitable for passing to ranger.New together with the
// Image's Transport.
func (i *Image) BlobURL(d v1.Hash) string {
	reg := i.Ref.Context().Registry
	return fmt.Sprintf("%s://%s/v2/%s/blobs/%s",
		reg.Scheme(), reg.Name(), i.Ref.Context().RepositoryStr(), d.String())
}

// ResolveDigest returns the manifest digest for ref, useful for resolving
// a tag to a digest without pulling layers.
func ResolveDigest(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Hash, error) {
	d, _, err := ResolveDigestAndPlatforms(ctx, ref, platform)
	return d, err
}

// PlatformDigest pairs a platform with the digest of its single-platform
// image manifest within an index.
type PlatformDigest struct {
	Platform v1.Platform
	Digest   v1.Hash
}

// ResolveDigestAndPlatforms is like ResolveDigest but also returns the
// list of platforms in the index manifest (when ref points at an index).
// For non-index refs, the platforms slice is nil.
func ResolveDigestAndPlatforms(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Hash, []PlatformDigest, error) {
	tr, err := buildTransport(ctx, ref)
	if err != nil {
		return v1.Hash{}, nil, err
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithTransport(tr), remote.WithPlatform(platform))
	if err != nil {
		return v1.Hash{}, nil, err
	}
	if !desc.MediaType.IsIndex() {
		return desc.Digest, nil, nil
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return v1.Hash{}, nil, err
	}
	mf, err := idx.IndexManifest()
	if err != nil {
		return v1.Hash{}, nil, err
	}
	var picked v1.Hash
	var pairs []PlatformDigest
	for _, m := range mf.Manifests {
		if m.Platform == nil {
			continue
		}
		pairs = append(pairs, PlatformDigest{Platform: *m.Platform, Digest: m.Digest})
		if m.Platform.OS == platform.OS && m.Platform.Architecture == platform.Architecture {
			picked = m.Digest
		}
	}
	if (picked == v1.Hash{}) {
		return v1.Hash{}, nil, fmt.Errorf("no manifest for %s/%s in index", platform.OS, platform.Architecture)
	}
	return picked, pairs, nil
}

// buildTransport returns an authenticated HTTP transport for ref's registry.
// If a disk cache has been registered via SetCache, the transport is wrapped
// with one that caches digest-keyed manifest + config responses on disk.
func buildTransport(ctx context.Context, ref name.Reference) (http.RoundTripper, error) {
	auth, err := authn.DefaultKeychain.Resolve(ref.Context())
	if err != nil {
		return nil, fmt.Errorf("resolve auth for %s: %w", ref.Context().RegistryStr(), err)
	}
	base := http.DefaultTransport
	if diskCache != nil {
		base = newDiskCachingTransport(base, diskCache)
	}
	tr, err := transport.NewWithContext(ctx, ref.Context().Registry, auth, base, []string{ref.Scope(transport.PullScope)})
	if err != nil {
		return nil, fmt.Errorf("authenticated transport for %s: %w", ref.Context().RegistryStr(), err)
	}
	return tr, nil
}

func imageFromIndex(idx v1.ImageIndex, platform v1.Platform) (v1.Image, error) {
	mf, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, m := range mf.Manifests {
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS == platform.OS && m.Platform.Architecture == platform.Architecture {
			return idx.Image(m.Digest)
		}
	}
	return nil, fmt.Errorf("no manifest for %s/%s", platform.OS, platform.Architecture)
}
