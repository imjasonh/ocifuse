package oci

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/imjasonh/ocifuse/internal/cache"
)

// diskCachingTransport wraps another http.RoundTripper and caches GET
// responses for digest-keyed manifest and image-config blobs in disk cache.
// Both are content-addressed and immutable, so caching by URL path is safe.
//
// We deliberately don't cache layer blobs here — those are large and reads
// arrive as Range requests; the chunk cache in internal/layer handles them.
// We also don't cache tag-keyed manifest lookups, because tags move.
type diskCachingTransport struct {
	base  http.RoundTripper
	cache *cache.Cache
}

func newDiskCachingTransport(base http.RoundTripper, c *cache.Cache) *diskCachingTransport {
	return &diskCachingTransport{base: base, cache: c}
}

// digestPathRE matches registry V2 paths whose final segment is a content
// digest, e.g. /v2/library/alpine/manifests/sha256:abc... or
// /v2/library/alpine/blobs/sha256:abc...
var digestPathRE = regexp.MustCompile(`^/v2/.+/(manifests|blobs)/sha256:[0-9a-f]{64}$`)

// cacheableContentType reports whether a response body for a
// digest-addressed URL should be cached. Manifests and image configs are
// small JSON blobs; layer blobs (also at /blobs/sha256:...) are not cached
// here.
func cacheableContentType(ct string) bool {
	ct = strings.ToLower(ct)
	if strings.Contains(ct, "manifest") {
		return true
	}
	if strings.Contains(ct, "image.index") {
		return true
	}
	if strings.Contains(ct, "image.config") || strings.Contains(ct, "container.image.v1+json") {
		return true
	}
	return false
}

func (t *diskCachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != "GET" || req.Header.Get("Range") != "" || !digestPathRE.MatchString(req.URL.Path) {
		return t.base.RoundTrip(req)
	}
	key := "registry" + req.URL.Path

	if entry, ok := t.loadCached(key); ok {
		return entry.toResponse(req), nil
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	ct := resp.Header.Get("Content-Type")
	if !cacheableContentType(ct) {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	t.storeCached(key, ct, body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// cachedEntry is the on-disk shape: a one-line content-type, a blank
// separator line, then the body. Manifests and configs are JSON, so
// human-readable on disk for debugging.
type cachedEntry struct {
	contentType string
	body        []byte
}

func (e cachedEntry) toResponse(req *http.Request) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", e.contentType)
	h.Set("Content-Length", itoa(len(e.body)))
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

func (t *diskCachingTransport) loadCached(key string) (cachedEntry, bool) {
	r, err := t.cache.Open(key)
	if err != nil {
		return cachedEntry{}, false
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		return cachedEntry{}, false
	}
	// First line is content-type, then blank line, then body.
	idx := bytes.Index(raw, []byte("\n\n"))
	if idx < 0 {
		return cachedEntry{}, false
	}
	return cachedEntry{
		contentType: string(raw[:idx]),
		body:        raw[idx+2:],
	}, true
}

func (t *diskCachingTransport) storeCached(key, contentType string, body []byte) {
	_ = t.cache.Write(key, func(w io.Writer) error {
		if _, err := io.WriteString(w, contentType); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return err
		}
		_, err := w.Write(body)
		return err
	})
}

func itoa(n int) string {
	// Tiny helper to avoid pulling strconv into the hot path comments.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
