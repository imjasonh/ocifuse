package mount_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/jonjohnsonjr/targz/tarfs"

	"github.com/imjasonh/ocifuse/internal/cache"
	"github.com/imjasonh/ocifuse/internal/layer"
	"github.com/imjasonh/ocifuse/internal/merge"
	"github.com/imjasonh/ocifuse/internal/oci"
)

// TestPipeline pushes a multi-layer image with whiteouts to an in-process
// registry and exercises the full resolve → index → merge → read pipeline,
// asserting that file contents and overlay/whiteout semantics are correct.
//
// FUSE mount itself is not exercised — that needs a kernel module — but
// every layer below FUSE is verified end-to-end.
func TestPipeline(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Layer 0: /etc/foo, /etc/bar, /usr/keep
	l0 := buildGzipLayer(t, []tarEntry{
		{name: "etc/foo", body: "L0-foo"},
		{name: "etc/bar", body: "L0-bar"},
		{name: "usr/keep", body: "kept"},
	})
	// Layer 1: replaces /etc/foo, whites out /etc/bar, adds /etc/baz
	l1 := buildGzipLayer(t, []tarEntry{
		{name: "etc/foo", body: "L1-foo"},
		{name: "etc/.wh.bar"},
		{name: "etc/baz", body: "L1-baz"},
	})

	img, err := mutate.AppendLayers(empty.Image, l0, l1)
	if err != nil {
		t.Fatal(err)
	}

	tag, err := name.NewTag(u.Host + "/test/image:latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}

	c, err := cache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ix := layer.NewIndexer(c, layer.NewChunkCache(0, 1<<20))

	ctx := context.Background()
	ref, err := name.ParseReference(u.Host + "/test/image:latest")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := oci.Resolve(ctx, ref, oci.DefaultPlatform)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	layers := make([]*layer.Layer, len(resolved.Layers))
	entries := make([][]*tarfs.Entry, len(resolved.Layers))
	for i, l := range resolved.Layers {
		d, err := l.Digest()
		if err != nil {
			t.Fatal(err)
		}
		lr, err := ix.Open(ctx, resolved.BlobURL(d), l, resolved.Transport)
		if err != nil {
			t.Fatalf("layer %d Open: %v", i, err)
		}
		layers[i] = lr
		entries[i] = lr.Entries
	}

	tree := merge.Merge(entries)

	cases := []struct {
		path     string
		exists   bool
		layerIdx int
		body     string
	}{
		{"/etc/foo", true, 1, "L1-foo"}, // L1 replaces L0
		{"/etc/bar", false, 0, ""},      // whited out
		{"/etc/baz", true, 1, "L1-baz"}, // added by L1
		{"/usr/keep", true, 0, "kept"},  // survives unchanged
	}
	for _, c := range cases {
		e, err := tree.Get(c.path)
		if !c.exists {
			if err == nil {
				t.Errorf("%s: expected absent, present at layer %d", c.path, e.LayerIdx)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if e.LayerIdx != c.layerIdx {
			t.Errorf("%s LayerIdx = %d, want %d", c.path, e.LayerIdx, c.layerIdx)
		}
		got, err := readPath(layers[e.LayerIdx], c.path)
		if err != nil {
			t.Errorf("%s read: %v", c.path, err)
			continue
		}
		if got != c.body {
			t.Errorf("%s = %q, want %q", c.path, got, c.body)
		}
	}

	// Whiteout marker itself must not appear.
	if _, err := tree.Get("/etc/.wh.bar"); err == nil {
		t.Errorf(".wh.bar marker leaked into merged tree")
	}
}

// readPath reads the bytes of an in-image path from the given layer via the
// tarfs.FS, so this exercises the gsip + ranger pipeline end-to-end (HTTP
// Range requests against the registry).
func readPath(l *layer.Layer, p string) (string, error) {
	f, err := l.FS.Open(strings.TrimPrefix(p, "/"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type tarEntry struct {
	name string // tar entry name, no leading slash
	body string // empty body → directory or whiteout marker
}

func buildGzipLayer(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Typeflag: tar.TypeReg,
			Size:     int64(len(e.body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %s: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write %s body: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	l, err := tarball.LayerFromReader(bytes.NewReader(raw.Bytes()))
	if err != nil {
		t.Fatalf("LayerFromReader: %v", err)
	}
	return l
}
