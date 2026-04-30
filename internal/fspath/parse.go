// Package fspath parses FUSE-relative paths into an OCI reference and an
// in-image path.
//
// The mount layout is:
//
//	<registry>/<repo...>/<ref>/<in-image-path>
//
// where <ref> is the first path segment containing ':' (tag) or '@' (digest).
// Anything before that segment names a registry plus repo path; anything after
// is the path inside the image.
//
// Docker Hub shorthand is honored: a leading segment that is not a hostname
// is fed to go-containerregistry's reference parser, which prepends
// index.docker.io/library/ as appropriate.
package fspath

import (
	"fmt"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Parsed is the result of parsing a FUSE-relative path.
//
// When Ref is nil the path lies in the registry/repo hierarchy above any
// resolved image; Intermediate holds the segments walked so far.
//
// When Ref is non-nil the path has crossed into an image; InImage is the
// POSIX path within that image, always rooted at "/" (e.g. "/etc/os-release"),
// and is "/" when the path stops exactly at the ref segment.
type Parsed struct {
	Intermediate []string
	Ref          name.Reference
	InImage      string
}

// Parse splits a slash-separated path (relative to the FUSE mount root) at
// the first segment containing ':' or '@', treating that segment as the OCI
// reference boundary. A ':' in the first segment is ignored when it looks
// like a registry port (host:digits) so that registries such as
// localhost:5000 round-trip correctly.
func Parse(p string) (Parsed, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return Parsed{}, nil
	}
	segs := strings.Split(p, "/")

	refIdx := -1
	for i, s := range segs {
		if i == 0 && isRegistrySegment(s) {
			continue
		}
		if strings.ContainsAny(s, ":@") {
			refIdx = i
			break
		}
	}

	if refIdx < 0 {
		return Parsed{Intermediate: segs}, nil
	}

	refStr := strings.Join(segs[:refIdx+1], "/")
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return Parsed{}, fmt.Errorf("parse ref %q: %w", refStr, err)
	}

	inImage := "/"
	if tail := segs[refIdx+1:]; len(tail) > 0 {
		inImage = "/" + path.Join(tail...)
	}
	return Parsed{Ref: ref, InImage: inImage}, nil
}

// isRegistrySegment reports whether s looks like a registry hostname (with
// optional port), and is used to suppress ref-boundary detection on the first
// segment when its ':' is really a host:port separator.
//
// Bare "localhost" is intentionally not recognized: go-containerregistry's
// reference parser treats it as a Docker Hub username, so a "localhost/foo:tag"
// path resolves to index.docker.io/localhost/foo:tag. Use "localhost:port" for
// a real local registry.
func isRegistrySegment(s string) bool {
	if strings.Contains(s, ".") {
		return true
	}
	if i := strings.LastIndex(s, ":"); i > 0 {
		port := s[i+1:]
		if port != "" && allDigits(port) {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
