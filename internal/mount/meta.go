package mount

import (
	"context"
	"path"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"

	"github.com/imjasonh/ocifuse/internal/layer"
	"github.com/imjasonh/ocifuse/internal/merge"
)

// MetaDirName is the synthetic directory injected at image and layer
// roots. The "@@" prefix avoids colliding with realistic filenames.
const MetaDirName = "@@meta"

// imageMetaDir is the @@meta directory at an image's root. Its children
// are "digest" (the image manifest digest, as text) and "layers" (a
// directory whose children are per-layer raw views).
type imageMetaDir struct {
	gofs.Inode
	fs *Filesystem
	ri *resolvedImage
}

var (
	_ gofs.NodeLookuper  = (*imageMetaDir)(nil)
	_ gofs.NodeReaddirer = (*imageMetaDir)(nil)
)

func (m *imageMetaDir) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	switch child {
	case "digest":
		return newTextFile(ctx, m.EmbeddedInode(), m.ri.img.Digest.String()+"\n", out), 0
	case "layers":
		d := &layersDir{fs: m.fs, ri: m.ri}
		out.Mode = gofuse.S_IFDIR
		out.Attr.Mode = gofuse.S_IFDIR | 0o755
		return m.NewInode(ctx, d, gofs.StableAttr{Mode: gofuse.S_IFDIR}), 0
	}
	return nil, syscall.ENOENT
}

func (m *imageMetaDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	return gofs.NewListDirStream([]gofuse.DirEntry{
		{Name: "digest", Mode: gofuse.S_IFREG},
		{Name: "layers", Mode: gofuse.S_IFDIR},
	}), 0
}

// layersDir lists each layer of an image as a child directory named by
// the layer's content digest. Each child is a layerDir at its own root.
type layersDir struct {
	gofs.Inode
	fs *Filesystem
	ri *resolvedImage
}

var (
	_ gofs.NodeLookuper  = (*layersDir)(nil)
	_ gofs.NodeReaddirer = (*layersDir)(nil)
)

func (l *layersDir) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	for _, ly := range l.ri.layers {
		if ly.Digest.String() == child {
			d := &layerDir{
				fs:    l.fs,
				layer: ly,
				tree:  merge.LayerTree(ly.Entries),
				path:  "/",
			}
			out.Mode = gofuse.S_IFDIR
			out.Attr.Mode = gofuse.S_IFDIR | 0o755
			return l.NewInode(ctx, d, gofs.StableAttr{Mode: gofuse.S_IFDIR}), 0
		}
	}
	return nil, syscall.ENOENT
}

func (l *layersDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	out := make([]gofuse.DirEntry, 0, len(l.ri.layers))
	for _, ly := range l.ri.layers {
		out = append(out, gofuse.DirEntry{Name: ly.Digest.String(), Mode: gofuse.S_IFDIR})
	}
	return gofs.NewListDirStream(out), 0
}

// layerDir is a directory within a single layer's raw tar. Whiteout
// markers (".wh.<name>", ".wh..wh..opq") appear as ordinary files. At
// path="/" it injects an @@meta sibling that exposes the layer's digest.
type layerDir struct {
	gofs.Inode
	fs    *Filesystem
	layer *layer.Layer
	tree  *merge.Tree
	path  string
}

var (
	_ gofs.NodeLookuper  = (*layerDir)(nil)
	_ gofs.NodeReaddirer = (*layerDir)(nil)
	_ gofs.NodeGetattrer = (*layerDir)(nil)
)

func (d *layerDir) Getattr(ctx context.Context, fh gofs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	if e, err := d.tree.Get(d.path); err == nil {
		setAttrFromHeader(&out.Attr, &e.Header, true)
	} else {
		out.Attr.Mode = gofuse.S_IFDIR | 0o755
	}
	return 0
}

func (d *layerDir) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if d.path == "/" && child == MetaDirName {
		m := &layerMetaDir{fs: d.fs, layer: d.layer}
		out.Mode = gofuse.S_IFDIR
		out.Attr.Mode = gofuse.S_IFDIR | 0o755
		return d.NewInode(ctx, m, gofs.StableAttr{Mode: gofuse.S_IFDIR}), 0
	}
	childPath := joinInImage(d.path, child)
	e, err := d.tree.Get(childPath)
	if err != nil {
		return nil, syscall.ENOENT
	}
	var embedder gofs.InodeEmbedder
	var mode uint32
	switch {
	case e.IsDir():
		embedder = &layerDir{fs: d.fs, layer: d.layer, tree: d.tree, path: childPath}
		mode = gofuse.S_IFDIR
	case e.IsSymlink():
		embedder = &gofs.MemSymlink{Data: []byte(e.Header.Linkname)}
		mode = gofuse.S_IFLNK
	default:
		embedder = &imageFile{fs: d.fs, layers: []*layer.Layer{d.layer}, entry: e}
		mode = gofuse.S_IFREG
	}
	setAttrFromHeader(&out.Attr, &e.Header, e.IsDir())
	out.Mode = mode
	return d.NewInode(ctx, embedder, gofs.StableAttr{Mode: mode}), 0
}

func (d *layerDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	children := d.tree.Children(d.path)
	out := make([]gofuse.DirEntry, 0, len(children)+1)
	if d.path == "/" {
		out = append(out, gofuse.DirEntry{Name: MetaDirName, Mode: gofuse.S_IFDIR})
	}
	for _, e := range children {
		de := gofuse.DirEntry{Name: path.Base(e.Path)}
		switch {
		case e.IsDir():
			de.Mode = gofuse.S_IFDIR
		case e.IsSymlink():
			de.Mode = gofuse.S_IFLNK
		default:
			de.Mode = gofuse.S_IFREG
		}
		out = append(out, de)
	}
	return gofs.NewListDirStream(out), 0
}

// layerMetaDir is the @@meta directory at a single layer's root. Its
// only child is "layer-digest", which contains the layer's digest.
type layerMetaDir struct {
	gofs.Inode
	fs    *Filesystem
	layer *layer.Layer
}

var (
	_ gofs.NodeLookuper  = (*layerMetaDir)(nil)
	_ gofs.NodeReaddirer = (*layerMetaDir)(nil)
)

func (m *layerMetaDir) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if child == "layer-digest" {
		return newTextFile(ctx, m.EmbeddedInode(), m.layer.Digest.String()+"\n", out), 0
	}
	return nil, syscall.ENOENT
}

func (m *layerMetaDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	return gofs.NewListDirStream([]gofuse.DirEntry{
		{Name: "layer-digest", Mode: gofuse.S_IFREG},
	}), 0
}

// newTextFile builds a small in-memory regular file with the given body.
// Used for the synthetic digest / layer-digest files.
func newTextFile(ctx context.Context, parent *gofs.Inode, body string, out *gofuse.EntryOut) *gofs.Inode {
	data := []byte(body)
	f := &gofs.MemRegularFile{
		Data: data,
		Attr: gofuse.Attr{Mode: 0o444, Size: uint64(len(data))},
	}
	out.Mode = gofuse.S_IFREG
	out.Attr.Mode = gofuse.S_IFREG | 0o444
	out.Attr.Size = uint64(len(data))
	return parent.NewInode(ctx, f, gofs.StableAttr{Mode: gofuse.S_IFREG})
}
