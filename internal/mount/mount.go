// Package mount implements the read-only OCI FUSE filesystem.
//
// The mount tree is lazy: the root directory has no eager children. When the
// kernel issues Lookup for a registry name (e.g. "gcr.io"), we synthesize an
// intermediate-prefix node. As successive segments arrive, we accumulate a
// path and feed it to fspath.Parse. When a segment containing ':' or '@'
// appears, we cross from the registry/repo prefix into an OCI ref:
//
//   - tag refs surface as symlinks pointing at the resolved digest sibling
//     (so re-reading the symlink picks up tag updates after a short TTL);
//   - digest refs resolve into an in-memory merged tree built from each
//     layer's tarfs index, with content reads served by HTTP Range requests
//     against the source registry.
package mount

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jonjohnsonjr/targz/tarfs"
	"golang.org/x/sync/singleflight"

	"github.com/imjasonh/ocifuse/internal/fspath"
	"github.com/imjasonh/ocifuse/internal/layer"
	"github.com/imjasonh/ocifuse/internal/merge"
	"github.com/imjasonh/ocifuse/internal/oci"
)

// Filesystem is the read-only OCI mount.
type Filesystem struct {
	Platform v1.Platform
	Indexer  *layer.Indexer
	// TagTTL bounds how stale a tag→digest mapping can be in the kernel's
	// dentry cache. After this elapses, the kernel re-Lookups the tag
	// segment and we re-fetch the manifest. Zero or negative falls back
	// to one minute.
	TagTTL time.Duration

	mu     sync.Mutex
	images map[v1.Hash]*resolvedImage
	sf     singleflight.Group // dedupes concurrent resolveImage calls per ref
	tagSF  singleflight.Group // dedupes concurrent tag→digest lookups
}

// resolvedImage caches the expensive parts of an image resolution: the
// manifest+config+layer descriptors, the indexed layers, and the merged tree.
// Keyed by manifest digest (immutable), so it's safe to reuse across paths
// and re-Lookups.
type resolvedImage struct {
	img    *oci.Image
	layers []*layer.Layer
	tree   *merge.Tree
}

// resolveImage returns a *resolvedImage for ref + platform, hitting the
// in-memory cache when possible. Concurrent calls for the same ref+platform
// share a single fetch.
func (f *Filesystem) resolveImage(ctx context.Context, ref name.Reference, platform v1.Platform) (*resolvedImage, error) {
	if d, ok := ref.(name.Digest); ok {
		if h, err := v1.NewHash(d.DigestStr()); err == nil {
			f.mu.Lock()
			ri, hit := f.images[h]
			f.mu.Unlock()
			if hit {
				return ri, nil
			}
		}
	}
	key := ref.String() + "@@" + platform.String()
	v, err, _ := f.sf.Do(key, func() (any, error) {
		img, err := oci.Resolve(ctx, ref, platform)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		if ri, ok := f.images[img.Digest]; ok {
			f.mu.Unlock()
			return ri, nil
		}
		f.mu.Unlock()

		layers, layerEntries, err := openLayers(ctx, f.Indexer, img)
		if err != nil {
			return nil, err
		}
		tree := merge.Merge(layerEntries)
		ri := &resolvedImage{img: img, layers: layers, tree: tree}

		f.mu.Lock()
		if existing, ok := f.images[img.Digest]; ok {
			f.mu.Unlock()
			return existing, nil
		}
		f.images[img.Digest] = ri
		f.mu.Unlock()
		return ri, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*resolvedImage), nil
}

// resolveTagDigest is the singleflight-deduped form of oci.ResolveDigestAndPlatforms.
// Returns the picked digest plus the list of platforms in the index
// (empty if ref doesn't point at an index).
func (f *Filesystem) resolveTagDigest(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Hash, []oci.PlatformDigest, error) {
	type result struct {
		digest    v1.Hash
		platforms []oci.PlatformDigest
	}
	key := ref.String() + "@@" + platform.String()
	v, err, _ := f.tagSF.Do(key, func() (any, error) {
		d, ps, err := oci.ResolveDigestAndPlatforms(ctx, ref, platform)
		if err != nil {
			return nil, err
		}
		return result{d, ps}, nil
	})
	if err != nil {
		return v1.Hash{}, nil, err
	}
	r := v.(result)
	return r.digest, r.platforms, nil
}

// Mount mounts the filesystem at mountpoint and returns the running server.
// The caller is expected to Wait() on the server.
func (f *Filesystem) Mount(mountpoint string, debug bool) (*gofuse.Server, error) {
	if f.images == nil {
		f.images = make(map[v1.Hash]*resolvedImage)
	}
	root := &rootNode{fs: f}
	hourTTL := time.Hour
	tagTTL := time.Minute
	opts := &gofs.Options{
		EntryTimeout:         &hourTTL,
		AttrTimeout:          &hourTTL,
		NegativeTimeout:      &tagTTL,
	}
	opts.Debug = debug
	return gofs.Mount(mountpoint, root, opts)
}

// rootNode is the mount root.
type rootNode struct {
	gofs.Inode
	fs *Filesystem
}

var _ gofs.NodeLookuper = (*rootNode)(nil)

func (r *rootNode) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	return resolveSegment(ctx, r.EmbeddedInode(), r.fs, "", child, out)
}

// intermediateNode represents a registry/repo prefix above any OCI ref.
type intermediateNode struct {
	gofs.Inode
	fs     *Filesystem
	prefix string // accumulated path (no leading slash), e.g. "index.docker.io/library"
}

var _ gofs.NodeLookuper = (*intermediateNode)(nil)

func (n *intermediateNode) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	return resolveSegment(ctx, n.EmbeddedInode(), n.fs, n.prefix, child, out)
}

// segmentKind classifies what a path segment resolves to before any
// expensive resolution (tag→digest lookup, image fetch) happens.
type segmentKind int

const (
	segIntermediate segmentKind = iota // still in registry/repo prefix
	segTagRef                          // ref-bearing segment, tag form
	segDigestRef                       // ref-bearing segment, digest form
)

// classifySegment is the pure half of segment resolution: given the
// accumulated path so far and the next segment, decide what kind of node
// it should become and return the parsed ref + per-ref platform (when
// applicable). Extracted from the kernel-bound resolveSegment so it can
// be unit-tested.
func classifySegment(accumulated, child string) (segmentKind, name.Reference, *v1.Platform, string, error) {
	full := child
	if accumulated != "" {
		full = accumulated + "/" + child
	}
	parsed, err := fspath.Parse(full)
	if err != nil {
		return 0, nil, nil, full, err
	}
	if parsed.Ref == nil {
		return segIntermediate, nil, nil, full, nil
	}
	if _, isDigest := parsed.Ref.(name.Digest); isDigest {
		return segDigestRef, parsed.Ref, parsed.Platform, full, nil
	}
	return segTagRef, parsed.Ref, parsed.Platform, full, nil
}

// tagSymlinkTarget is the relative symlink target written for a tag ref:
// the basename of the repo joined with the digest, so the symlink points
// at its digest sibling in the same directory.
func tagSymlinkTarget(ref name.Reference, digest v1.Hash) string {
	return path.Base(ref.Context().RepositoryStr()) + "@" + digest.String()
}

// resolveSegment walks one path segment forward. If the result is still
// inside the registry/repo prefix it returns an intermediateNode; if it has
// crossed into an OCI tag it returns a symlink to the digest sibling; if it
// has crossed into a digest it resolves the image and returns its merged
// root.
func resolveSegment(ctx context.Context, parent *gofs.Inode, fsys *Filesystem, accumulated, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	kind, ref, plat, full, err := classifySegment(accumulated, child)
	if err != nil {
		return nil, syscall.ENOENT
	}
	switch kind {
	case segIntermediate:
		n := &intermediateNode{fs: fsys, prefix: full}
		out.Mode = gofuse.S_IFDIR
		out.Attr.Mode = gofuse.S_IFDIR | 0o755
		return parent.NewInode(ctx, n, gofs.StableAttr{Mode: gofuse.S_IFDIR}), 0
	case segTagRef:
		return tagSymlink(ctx, parent, fsys, ref, plat, out)
	case segDigestRef:
		return imageRoot(ctx, parent, fsys, ref, plat, out)
	}
	return nil, syscall.EIO
}

// platformOr returns p if non-nil, else the mount-wide default.
func (f *Filesystem) platformOr(p *v1.Platform) v1.Platform {
	if p != nil {
		return *p
	}
	return f.Platform
}

// tagSymlink resolves a tag ref to a digest and returns a symlink whose
// target is the digest sibling (e.g. "ubuntu@sha256:..."). If the ref
// points at a multi-arch index, sibling "<repo-base>:<tag>~<os>-<arch>"
// symlinks are AddChild'd to the parent so future readdir + tab
// completion can enumerate the platform variants.
func tagSymlink(ctx context.Context, parent *gofs.Inode, fsys *Filesystem, ref name.Reference, plat *v1.Platform, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	t0 := time.Now()
	platform := fsys.platformOr(plat)
	digest, platforms, err := fsys.resolveTagDigest(ctx, ref, platform)
	if err != nil {
		clog.FromContext(ctx).Error("tag resolve", "ref", ref.String(), "err", err)
		return nil, syscall.EIO
	}
	clog.FromContext(ctx).Debug("resolved tag", "ref", ref.String(), "digest", digest.String(), "platforms", len(platforms), "took", time.Since(t0))

	target := tagSymlinkTarget(ref, digest)
	sym := &gofs.MemSymlink{Data: []byte(target)}
	out.Mode = gofuse.S_IFLNK
	out.Attr.Mode = gofuse.S_IFLNK | 0o777
	out.Attr.Size = uint64(len(target))
	ttl := fsys.TagTTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	out.SetEntryTimeout(ttl)

	if len(platforms) > 0 {
		addPlatformSiblings(ctx, parent, ref, plat, platforms)
	}
	return parent.NewInode(ctx, sym, gofs.StableAttr{Mode: gofuse.S_IFLNK}), 0
}

// addPlatformSiblings AddChilds a "<repo-base>:<tag>~<os>-<arch>" symlink
// to parent for each platform in the index, except the one matching the
// originating Lookup (which the framework will register itself). Symlinks
// for already-present siblings are skipped.
func addPlatformSiblings(ctx context.Context, parent *gofs.Inode, ref name.Reference, current *v1.Platform, platforms []oci.PlatformDigest) {
	tag, ok := ref.(name.Tag)
	if !ok {
		return
	}
	base := path.Base(ref.Context().RepositoryStr())
	for _, pd := range platforms {
		if current != nil && samePlatform(*current, pd.Platform) {
			continue
		}
		spec := pd.Platform.OS + "-" + pd.Platform.Architecture
		if pd.Platform.Variant != "" {
			spec += "-" + pd.Platform.Variant
		}
		name := base + ":" + tag.TagStr() + "~" + spec
		if parent.GetChild(name) != nil {
			continue
		}
		target := base + "@" + pd.Digest.String()
		sibling := &gofs.MemSymlink{Data: []byte(target)}
		ino := parent.NewPersistentInode(ctx, sibling, gofs.StableAttr{Mode: gofuse.S_IFLNK})
		parent.AddChild(name, ino, false)
	}
}

func samePlatform(a, b v1.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}

// imageRoot resolves a digest ref into a fully merged image and returns its
// root directory inode.
func imageRoot(ctx context.Context, parent *gofs.Inode, fsys *Filesystem, ref name.Reference, plat *v1.Platform, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	t0 := time.Now()
	ri, err := fsys.resolveImage(ctx, ref, fsys.platformOr(plat))
	if err != nil {
		clog.FromContext(ctx).Error("image resolve", "ref", ref.String(), "err", err)
		return nil, syscall.EIO
	}
	clog.FromContext(ctx).Debug("resolved image", "ref", ref.String(), "layers", len(ri.layers), "took", time.Since(t0))
	d := &imageDir{
		fs:   fsys,
		ri:   ri,
		tree: ri.tree,
		path: "/",
	}
	out.Mode = gofuse.S_IFDIR
	out.Attr.Mode = gofuse.S_IFDIR | 0o755
	return parent.NewInode(ctx, d, gofs.StableAttr{Mode: gofuse.S_IFDIR}), 0
}

func openLayers(ctx context.Context, ix *layer.Indexer, img *oci.Image) ([]*layer.Layer, [][]*tarfs.Entry, error) {
	out := make([]*layer.Layer, len(img.Layers))
	entries := make([][]*tarfs.Entry, len(img.Layers))
	for i, l := range img.Layers {
		d, err := l.Digest()
		if err != nil {
			return nil, nil, err
		}
		url := img.BlobURL(d)
		lr, err := ix.Open(ctx, url, l, img.Transport)
		if err != nil {
			return nil, nil, fmt.Errorf("index layer %s: %w", d, err)
		}
		out[i] = lr
		entries[i] = lr.Entries
	}
	return out, entries, nil
}

// imageDir is a directory within (or at the root of) a resolved image.
// At path="/" it also surfaces the synthetic @@meta directory.
type imageDir struct {
	gofs.Inode
	fs     *Filesystem
	ri     *resolvedImage
	tree   *merge.Tree
	path   string // absolute in-image path; "/" for image root
}

var (
	_ gofs.NodeLookuper   = (*imageDir)(nil)
	_ gofs.NodeReaddirer  = (*imageDir)(nil)
	_ gofs.NodeGetattrer  = (*imageDir)(nil)
)

func (d *imageDir) Getattr(ctx context.Context, fh gofs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	if e, err := d.tree.Get(d.path); err == nil {
		setAttrFromHeader(&out.Attr, &e.Header, true)
	} else {
		out.Attr.Mode = gofuse.S_IFDIR | 0o755
	}
	return 0
}

func (d *imageDir) Lookup(ctx context.Context, child string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if d.path == "/" && child == MetaDirName {
		m := &imageMetaDir{fs: d.fs, ri: d.ri}
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
		embedder = &imageDir{fs: d.fs, ri: d.ri, tree: d.tree, path: childPath}
		mode = gofuse.S_IFDIR
	case e.IsSymlink():
		embedder = &gofs.MemSymlink{Data: []byte(e.Header.Linkname)}
		mode = gofuse.S_IFLNK
	default:
		embedder = &imageFile{fs: d.fs, layers: d.ri.layers, entry: e}
		mode = gofuse.S_IFREG
	}
	setAttrFromHeader(&out.Attr, &e.Header, e.IsDir())
	out.Mode = mode
	return d.NewInode(ctx, embedder, gofs.StableAttr{Mode: mode}), 0
}

func (d *imageDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
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

// imageFile is a regular file within a resolved image.
type imageFile struct {
	gofs.Inode
	fs     *Filesystem
	layers []*layer.Layer
	entry  *merge.Entry
}

var (
	_ gofs.NodeOpener    = (*imageFile)(nil)
	_ gofs.NodeReader    = (*imageFile)(nil)
	_ gofs.NodeGetattrer = (*imageFile)(nil)
)

func (f *imageFile) Getattr(ctx context.Context, fh gofs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	setAttrFromHeader(&out.Attr, &f.entry.Header, false)
	return 0
}

func (f *imageFile) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if f.entry.LayerIdx < 0 || f.entry.LayerIdx >= len(f.layers) {
		return nil, 0, syscall.EIO
	}
	layer := f.layers[f.entry.LayerIdx]
	tname := strings.TrimPrefix(f.entry.Path, "/")
	file, err := layer.FS.Open(tname)
	if err != nil {
		return nil, 0, syscall.EIO
	}
	return &fileHandle{file: file}, gofuse.FOPEN_KEEP_CACHE, 0
}

func (f *imageFile) Read(ctx context.Context, fh gofs.FileHandle, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	h, ok := fh.(*fileHandle)
	if !ok {
		return nil, syscall.EIO
	}
	ra, ok := h.file.(io.ReaderAt)
	if !ok {
		return nil, syscall.EIO
	}
	n, err := ra.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return gofuse.ReadResultData(dest[:n]), 0
}

type fileHandle struct {
	file iofs.File
}

func joinInImage(parent, child string) string {
	if parent == "/" {
		return "/" + child
	}
	return parent + "/" + child
}

func setAttrFromHeader(out *gofuse.Attr, h *tar.Header, isDir bool) {
	out.Mode = uint32(h.Mode) & 0o7777
	if out.Mode == 0 {
		if isDir {
			out.Mode = 0o755
		} else {
			out.Mode = 0o644
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		out.Mode |= gofuse.S_IFDIR
	case tar.TypeSymlink:
		out.Mode |= gofuse.S_IFLNK
	default:
		out.Mode |= gofuse.S_IFREG
	}
	out.Size = uint64(h.Size)
	out.Uid = uint32(h.Uid)
	out.Gid = uint32(h.Gid)
	if !h.ModTime.IsZero() {
		out.Mtime = uint64(h.ModTime.Unix())
		out.Atime = out.Mtime
		out.Ctime = out.Mtime
	}
	out.Nlink = 1
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
}
