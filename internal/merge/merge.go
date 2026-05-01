// Package merge folds N layer entry lists into a single in-image tree,
// applying OCI whiteouts (".wh.<name>") and opaque markers (".wh..wh..opq")
// bottom-up per the image-spec.
//
// The result is a static tree keyed by absolute in-image path ("/etc/passwd")
// that records, for each entry, which layer it came from plus the entry's
// offset and tar header. Reads dispatch back to that layer's tarfs.FS.
package merge

import (
	"archive/tar"
	"path"
	"sort"
	"strings"

	"github.com/jonjohnsonjr/targz/tarfs"

	"io/fs"
)

const (
	whPrefix = ".wh."
	whOpaque = ".wh..wh..opq"
)

// Entry describes a single inode in the merged tree.
type Entry struct {
	Path     string     // absolute in-image path ("/etc/os-release"); "/" for root.
	Header   tar.Header // copied from the layer-of-origin tar header
	LayerIdx int        // index into the layers slice passed to Merge
}

func (e *Entry) IsDir() bool {
	switch e.Header.Typeflag {
	case tar.TypeDir:
		return true
	}
	return e.Path == "/"
}

func (e *Entry) IsSymlink() bool { return e.Header.Typeflag == tar.TypeSymlink }
func (e *Entry) IsHardlink() bool { return e.Header.Typeflag == tar.TypeLink }

// Tree is the merged in-image tree.
type Tree struct {
	entries  map[string]*Entry
	children map[string][]string
}

// Get returns the entry at p (absolute in-image path) or fs.ErrNotExist.
func (t *Tree) Get(p string) (*Entry, error) {
	p = canon(p)
	e, ok := t.entries[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return e, nil
}

// Children returns the immediate children of p, sorted by full path.
func (t *Tree) Children(p string) []*Entry {
	p = canon(p)
	names := t.children[p]
	out := make([]*Entry, 0, len(names))
	for _, n := range names {
		if e, ok := t.entries[n]; ok {
			out = append(out, e)
		}
	}
	return out
}

// LayerTree builds a tree from a single layer's entries WITHOUT applying
// whiteout semantics. Whiteout markers (".wh.<name>", ".wh..wh..opq")
// appear as ordinary files. Missing parent dirs are synthesized. This is
// the per-layer view used under @@meta/layers/<digest>/.
func LayerTree(entries []*tarfs.Entry) *Tree {
	tree := &Tree{
		entries: map[string]*Entry{
			"/": {Path: "/", Header: tar.Header{Typeflag: tar.TypeDir, Name: "/", Mode: 0o755}},
		},
		children: map[string][]string{},
	}
	for _, e := range entries {
		full := canon("/" + e.Filename)
		tree.entries[full] = &Entry{
			Path:     full,
			Header:   e.Header,
			LayerIdx: 0,
		}
	}
	synthesizeParents(tree)
	for p := range tree.entries {
		if p == "/" {
			continue
		}
		parent := canon(path.Dir(p))
		tree.children[parent] = append(tree.children[parent], p)
	}
	for parent := range tree.children {
		sort.Strings(tree.children[parent])
	}
	return tree
}

// Merge folds layers (lowest first) into a single tree.
func Merge(layers [][]*tarfs.Entry) *Tree {
	tree := &Tree{
		entries: map[string]*Entry{
			"/": {Path: "/", Header: tar.Header{Typeflag: tar.TypeDir, Name: "/", Mode: 0o755}},
		},
		children: map[string][]string{},
	}

	for li, entries := range layers {
		applyLayer(tree, entries, li)
	}

	synthesizeParents(tree)

	for p := range tree.entries {
		if p == "/" {
			continue
		}
		parent := canon(path.Dir(p))
		tree.children[parent] = append(tree.children[parent], p)
	}
	for parent := range tree.children {
		sort.Strings(tree.children[parent])
	}
	return tree
}

// applyLayer applies one layer's entries to tree. Whiteouts in this layer
// affect what's already in tree (i.e. lower layers); same-layer entries are
// added afterwards. We sort the layer's entries by path so that whiteouts
// (which sort early via the .wh. prefix) are processed first within each dir.
func applyLayer(tree *Tree, entries []*tarfs.Entry, layerIdx int) {
	sorted := make([]*tarfs.Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Filename < sorted[j].Filename })

	for _, e := range sorted {
		full := canon("/" + e.Filename)
		base := path.Base(e.Filename)
		dir := canon("/" + path.Dir(e.Filename))

		switch {
		case base == whOpaque:
			deleteChildren(tree, dir)
		case strings.HasPrefix(base, whPrefix):
			target := canon(path.Join(dir, strings.TrimPrefix(base, whPrefix)))
			deleteSubtree(tree, target)
		default:
			tree.entries[full] = &Entry{
				Path:     full,
				Header:   e.Header,
				LayerIdx: layerIdx,
			}
		}
	}
}

// synthesizeParents adds Typeflag=TypeDir entries for any path whose parent is
// missing from the tree. Permissive 0755 mode. LayerIdx -1 marks them
// synthetic — they have no backing tar data and must not be Open'd for read.
func synthesizeParents(tree *Tree) {
	missing := map[string]struct{}{}
	for p := range tree.entries {
		if p == "/" {
			continue
		}
		for parent := canon(path.Dir(p)); parent != "/"; parent = canon(path.Dir(parent)) {
			if _, ok := tree.entries[parent]; !ok {
				missing[parent] = struct{}{}
			}
		}
	}
	for p := range missing {
		tree.entries[p] = &Entry{
			Path:     p,
			Header:   tar.Header{Typeflag: tar.TypeDir, Name: p, Mode: 0o755},
			LayerIdx: -1,
		}
	}
}

func deleteSubtree(tree *Tree, p string) {
	delete(tree.entries, p)
	prefix := p + "/"
	for k := range tree.entries {
		if strings.HasPrefix(k, prefix) {
			delete(tree.entries, k)
		}
	}
}

func deleteChildren(tree *Tree, p string) {
	prefix := canon(p)
	if prefix != "/" {
		prefix += "/"
	}
	for k := range tree.entries {
		if k == p {
			continue
		}
		if strings.HasPrefix(k, prefix) {
			delete(tree.entries, k)
		}
	}
}

// canon normalizes a path to leading-slash absolute with no trailing slash.
// "" and "." both map to "/".
func canon(p string) string {
	if p == "" || p == "." {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = path.Clean(p)
	return p
}
