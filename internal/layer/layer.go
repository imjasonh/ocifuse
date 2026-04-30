// Package layer indexes OCI layer blobs for random access via HTTP Range
// requests. The first access streams the gzipped tar once to build gsip
// checkpoints and a tarfs TOC, both persisted by layer digest. Subsequent
// accesses skip that pass and serve file reads with bounded byte ranges.
package layer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/ranger"
	"github.com/jonjohnsonjr/targz/tarfs"

	"github.com/imjasonh/ocifuse/internal/cache"
)

type Indexer struct {
	cache  *cache.Cache
	chunks *ChunkCache
}

// NewIndexer constructs an indexer that persists tar/gsip indexes to disk
// (via cache) and serves compressed-byte reads through chunks (a global
// LRU-bounded in-memory cache). chunks may be nil to disable in-memory
// caching entirely; the layer will still index correctly but every Range
// region will be re-fetched on each read.
func NewIndexer(c *cache.Cache, chunks *ChunkCache) *Indexer {
	return &Indexer{cache: c, chunks: chunks}
}

// Layer is a layer with a tarfs ready for random-access file reads, plus the
// flat entry list extracted from the tar (so callers can enumerate even when
// the layer omits explicit parent-dir entries — common in OCI).
type Layer struct {
	Digest  v1.Hash
	Size    int64
	FS      *tarfs.FS
	Entries []*tarfs.Entry
}

// Open returns an indexed Layer, building and persisting indexes on first
// access. blobURL must address the gzipped layer blob; tr is an authenticated
// transport scoped to the source registry.
func (ix *Indexer) Open(ctx context.Context, blobURL string, l v1.Layer, tr http.RoundTripper) (*Layer, error) {
	digest, err := l.Digest()
	if err != nil {
		return nil, err
	}
	size, err := l.Size()
	if err != nil {
		return nil, err
	}

	// Wrap ranger in the shared chunk cache so repeat reads of the same
	// compressed regions (gsip rewinding to a checkpoint, nearby files
	// sharing a checkpoint window, ditto across layers and across mounts
	// in the same process) hit memory instead of re-issuing Range
	// requests. Partial-fetch semantics are preserved: chunks are
	// populated only when something asks for bytes in their range.
	var rra io.ReaderAt = ranger.New(ctx, blobURL, tr)
	if ix.chunks != nil {
		rra = ix.chunks.ForLayer(digest, rra, size)
	}
	gsipKey := key(digest, "gsip.idx")
	tocKey := key(digest, "tarfs.toc")

	if fsys, entries, ok, err := ix.loadCached(rra, size, gsipKey, tocKey); err != nil {
		return nil, err
	} else if ok {
		return &Layer{Digest: digest, Size: size, FS: fsys, Entries: entries}, nil
	}

	zr, err := gsip.NewReader(rra, size)
	if err != nil {
		return nil, fmt.Errorf("gsip reader for %s: %w", digest, err)
	}
	// Single sequential pass: gsip records checkpoints, tarfs.Index records
	// each tar entry's offset. Pass MaxInt64 — the tar end-of-archive marker
	// stops the read.
	sr := io.NewSectionReader(zr, 0, 1<<63-1)
	entries, err := tarfs.Index(sr)
	if err != nil {
		return nil, fmt.Errorf("scan tar for %s: %w", digest, err)
	}

	var tocBuf bytes.Buffer
	if err := json.NewEncoder(&tocBuf).Encode(tarfs.TOC{Entries: entries}); err != nil {
		return nil, fmt.Errorf("encode tarfs TOC for %s: %w", digest, err)
	}
	tocBytes := tocBuf.Bytes()

	if err := ix.cache.Write(tocKey, func(w io.Writer) error {
		_, err := w.Write(tocBytes)
		return err
	}); err != nil {
		return nil, fmt.Errorf("persist tarfs TOC for %s: %w", digest, err)
	}
	if err := ix.cache.Write(gsipKey, func(w io.Writer) error { return zr.Encode(w) }); err != nil {
		return nil, fmt.Errorf("persist gsip index for %s: %w", digest, err)
	}

	fsys, err := tarfs.Decode(zr, bytes.NewReader(tocBytes))
	if err != nil {
		return nil, fmt.Errorf("build tarfs from TOC for %s: %w", digest, err)
	}
	return &Layer{Digest: digest, Size: size, FS: fsys, Entries: entries}, nil
}

func (ix *Indexer) loadCached(rra io.ReaderAt, size int64, gsipKey, tocKey string) (*tarfs.FS, []*tarfs.Entry, bool, error) {
	if !ix.cache.Has(gsipKey) || !ix.cache.Has(tocKey) {
		return nil, nil, false, nil
	}
	gz, err := ix.cache.Open(gsipKey)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	defer gz.Close()
	zr, err := gsip.Decode(rra, size, gz)
	if err != nil {
		return nil, nil, false, fmt.Errorf("decode gsip index: %w", err)
	}
	tocBytes, err := readAll(ix.cache, tocKey)
	if err != nil {
		return nil, nil, false, err
	}

	var toc tarfs.TOC
	if err := json.Unmarshal(tocBytes, &toc); err != nil {
		return nil, nil, false, fmt.Errorf("decode tarfs TOC: %w", err)
	}
	fsys, err := tarfs.Decode(zr, bytes.NewReader(tocBytes))
	if err != nil {
		return nil, nil, false, fmt.Errorf("build tarfs from TOC: %w", err)
	}
	return fsys, toc.Entries, true, nil
}

func readAll(c *cache.Cache, key string) ([]byte, error) {
	r, err := c.Open(key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func key(d v1.Hash, name string) string {
	return "layers/" + d.Algorithm + "/" + d.Hex + "/" + name
}
