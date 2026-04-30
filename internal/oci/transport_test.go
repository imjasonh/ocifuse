package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/imjasonh/ocifuse/internal/cache"
)

func TestDiskCachingTransport_OnlyCachesDigestPaths(t *testing.T) {
	cases := []struct {
		path        string
		contentType string
		wantCache   bool
	}{
		{"/v2/library/alpine/manifests/sha256:0000000000000000000000000000000000000000000000000000000000000000", "application/vnd.oci.image.manifest.v1+json", true},
		{"/v2/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000", "application/vnd.oci.image.config.v1+json", true},
		{"/v2/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000", "application/octet-stream", false}, // a layer blob
		{"/v2/library/alpine/manifests/latest", "application/vnd.oci.image.manifest.v1+json", false},                                          // tag — must not cache
		{"/v2/", "application/json", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var hits atomic.Int64
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", tc.contentType)
				io.WriteString(w, "BODY")
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			c, err := cache.New(t.TempDir(), 0)
			if err != nil {
				t.Fatal(err)
			}
			tr := newDiskCachingTransport(http.DefaultTransport, c)
			cli := &http.Client{Transport: tr}

			doGet := func() string {
				req, _ := http.NewRequest("GET", srv.URL+tc.path, nil)
				resp, err := cli.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return string(body)
			}

			b1 := doGet()
			b2 := doGet()
			if b1 != "BODY" || b2 != "BODY" {
				t.Errorf("body mismatch: %q %q", b1, b2)
			}

			wantHits := int64(2)
			if tc.wantCache {
				wantHits = 1
			}
			if got := hits.Load(); got != wantHits {
				t.Errorf("upstream hits = %d, want %d", got, wantHits)
			}
		})
	}
}

func TestDiskCachingTransport_PreservesContentType(t *testing.T) {
	const ct = "application/vnd.oci.image.manifest.v1+json"
	const path = "/v2/library/alpine/manifests/sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const body = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c, err := cache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tr := newDiskCachingTransport(http.DefaultTransport, c)
	cli := &http.Client{Transport: tr}

	// Prime cache.
	resp, err := cli.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Replay from cache: response must carry the same Content-Type so
	// go-containerregistry can route the manifest to the right parser.
	resp, err = cli.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != ct {
		t.Errorf("Content-Type = %q, want %q", got, ct)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(gotBody), "schemaVersion") {
		t.Errorf("body missing expected content: %q", gotBody)
	}
}
