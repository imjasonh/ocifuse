package merge

import (
	"archive/tar"
	"bytes"
	"sort"
	"testing"

	"github.com/jonjohnsonjr/targz/tarfs"
)

// buildLayer writes a tar with the given entries into a buffer and returns a
// flat entry list via tarfs.Index — i.e. the same shape Phase 3 produces for
// real layers.
func buildLayer(t *testing.T, entries []tar.Header, contents map[string]string) []*tarfs.Entry {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		body := []byte(contents[h.Name])
		hcopy := h
		if hcopy.Typeflag == 0 && len(body) > 0 {
			hcopy.Typeflag = tar.TypeReg
		}
		if hcopy.Typeflag == tar.TypeReg {
			hcopy.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&hcopy); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if hcopy.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("Write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	es, err := tarfs.Index(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("tarfs.Index: %v", err)
	}
	return es
}

func TestMerge_LayerOverlay(t *testing.T) {
	l0 := buildLayer(t, []tar.Header{
		{Name: "a"}, {Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/x"}, {Name: "b/y"},
	}, map[string]string{"a": "A0", "b/x": "X0", "b/y": "Y0"})
	// L1 deliberately omits "b/" — OCI layers may skip parent-dir entries.
	l1 := buildLayer(t, []tar.Header{
		{Name: "b/y"}, {Name: "c"},
	}, map[string]string{"b/y": "Y1", "c": "C1"})

	tree := Merge([][]*tarfs.Entry{l0, l1})

	cases := []struct {
		path     string
		layerIdx int
	}{
		{"/a", 0}, {"/b/x", 0}, {"/b/y", 1}, {"/c", 1},
	}
	for _, c := range cases {
		e, err := tree.Get(c.path)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if e.LayerIdx != c.layerIdx {
			t.Errorf("%s LayerIdx = %d, want %d", c.path, e.LayerIdx, c.layerIdx)
		}
	}
	// /b should still be present (synthesized from L0's explicit dir entry).
	if _, err := tree.Get("/b"); err != nil {
		t.Errorf("/b missing: %v", err)
	}
}

func TestMerge_FileWhiteout(t *testing.T) {
	l0 := buildLayer(t, []tar.Header{
		{Name: "a"}, {Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/x"}, {Name: "b/y"},
	}, map[string]string{"a": "A", "b/x": "X", "b/y": "Y"})
	l1 := buildLayer(t, []tar.Header{
		{Name: "b/.wh.x"},
	}, nil)

	tree := Merge([][]*tarfs.Entry{l0, l1})
	if _, err := tree.Get("/b/x"); err == nil {
		t.Errorf("/b/x should be removed by whiteout")
	}
	if _, err := tree.Get("/b/y"); err != nil {
		t.Errorf("/b/y should survive: %v", err)
	}
	if _, err := tree.Get("/a"); err != nil {
		t.Errorf("/a should survive: %v", err)
	}
	if _, err := tree.Get("/b/.wh.x"); err == nil {
		t.Errorf(".wh.x marker should not appear in merged tree")
	}
}

func TestMerge_DirectoryWhiteout(t *testing.T) {
	l0 := buildLayer(t, []tar.Header{
		{Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/x"}, {Name: "b/y"}, {Name: "b/sub/", Typeflag: tar.TypeDir}, {Name: "b/sub/z"},
	}, map[string]string{"b/x": "X", "b/y": "Y", "b/sub/z": "Z"})
	l1 := buildLayer(t, []tar.Header{
		{Name: ".wh.b"},
	}, nil)

	tree := Merge([][]*tarfs.Entry{l0, l1})
	for _, p := range []string{"/b", "/b/x", "/b/y", "/b/sub", "/b/sub/z"} {
		if _, err := tree.Get(p); err == nil {
			t.Errorf("%s should be removed by directory whiteout", p)
		}
	}
}

func TestMerge_OpaqueDirectory(t *testing.T) {
	l0 := buildLayer(t, []tar.Header{
		{Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/x"}, {Name: "b/y"},
	}, map[string]string{"b/x": "X", "b/y": "Y"})
	l1 := buildLayer(t, []tar.Header{
		{Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/.wh..wh..opq"}, {Name: "b/z"},
	}, map[string]string{"b/z": "Z"})

	tree := Merge([][]*tarfs.Entry{l0, l1})
	if _, err := tree.Get("/b/x"); err == nil {
		t.Errorf("/b/x should be hidden by opaque marker")
	}
	if _, err := tree.Get("/b/y"); err == nil {
		t.Errorf("/b/y should be hidden by opaque marker")
	}
	if _, err := tree.Get("/b/z"); err != nil {
		t.Errorf("/b/z should be present: %v", err)
	}
	if _, err := tree.Get("/b/.wh..wh..opq"); err == nil {
		t.Errorf("opaque marker should not appear in merged tree")
	}
}

func TestMerge_Children(t *testing.T) {
	l0 := buildLayer(t, []tar.Header{
		{Name: "a"}, {Name: "b/", Typeflag: tar.TypeDir}, {Name: "b/x"}, {Name: "b/y"},
	}, map[string]string{"a": "A", "b/x": "X", "b/y": "Y"})

	tree := Merge([][]*tarfs.Entry{l0})

	var rootNames []string
	for _, e := range tree.Children("/") {
		rootNames = append(rootNames, e.Path)
	}
	sort.Strings(rootNames)
	want := []string{"/a", "/b"}
	if !equal(rootNames, want) {
		t.Errorf("root children = %v, want %v", rootNames, want)
	}

	var bNames []string
	for _, e := range tree.Children("/b") {
		bNames = append(bNames, e.Path)
	}
	sort.Strings(bNames)
	want = []string{"/b/x", "/b/y"}
	if !equal(bNames, want) {
		t.Errorf("/b children = %v, want %v", bNames, want)
	}
}

func TestLayerTree_PreservesWhiteouts(t *testing.T) {
	// LayerTree must NOT filter whiteout markers — they're real entries
	// in the per-layer view.
	l := buildLayer(t, []tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir},
		{Name: "etc/foo"},
		{Name: "etc/.wh.bar"},
		{Name: "etc/.wh..wh..opq"},
	}, map[string]string{"etc/foo": "F"})

	tree := LayerTree(l)
	for _, p := range []string{"/etc/foo", "/etc/.wh.bar", "/etc/.wh..wh..opq"} {
		if _, err := tree.Get(p); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
}

func TestLayerTree_SynthesizesMissingParent(t *testing.T) {
	l := buildLayer(t, []tar.Header{
		{Name: "deep/nested/file"},
	}, map[string]string{"deep/nested/file": "X"})
	tree := LayerTree(l)
	for _, p := range []string{"/deep", "/deep/nested", "/deep/nested/file"} {
		if _, err := tree.Get(p); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
}

func TestMerge_SynthesizesMissingParent(t *testing.T) {
	// Layer has b/y but no explicit b/ entry; merged tree must still expose /b.
	l0 := buildLayer(t, []tar.Header{
		{Name: "b/y"},
	}, map[string]string{"b/y": "Y"})

	tree := Merge([][]*tarfs.Entry{l0})
	b, err := tree.Get("/b")
	if err != nil {
		t.Fatalf("/b synthesized parent missing: %v", err)
	}
	if !b.IsDir() {
		t.Errorf("/b should be a directory")
	}
	if b.LayerIdx != -1 {
		t.Errorf("synthesized parent LayerIdx = %d, want -1", b.LayerIdx)
	}
	if _, err := tree.Get("/b/y"); err != nil {
		t.Errorf("/b/y missing: %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
