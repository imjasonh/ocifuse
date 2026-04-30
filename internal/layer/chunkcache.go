package layer

import (
	"container/list"
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ChunkCache is a global LRU over compressed-byte chunks shared across all
// layers a process touches. Per-layer "views" (returned by ForLayer) wrap an
// underlying io.ReaderAt (typically a Range-capable HTTP reader) and lazily
// populate the global cache as chunks are fetched. Total cache size is held
// at or below cap by evicting least-recently-used chunks.
//
// The partial-fetch contract is preserved: chunks are only inserted when
// some caller actually asks for bytes in their range. Eviction is purely a
// memory-bound, not a network-bound.
type ChunkCache struct {
	cap       int64 // 0 disables eviction (unbounded)
	chunkSize int64

	mu    sync.Mutex
	used  int64
	items map[chunkKey]*lruEntry
	lru   *list.List // most-recently-used at Front, least at Back
}

type chunkKey struct {
	layer v1.Hash
	idx   int64
}

type lruEntry struct {
	key  chunkKey
	data []byte
	elem *list.Element
}

// NewChunkCache returns a global chunk cache with the given total-byte cap.
// chunkSize controls the size of fetched compressed-byte windows; 1MB is a
// good default. cap=0 disables eviction.
func NewChunkCache(capBytes, chunkSize int64) *ChunkCache {
	if chunkSize <= 0 {
		chunkSize = 1 << 20
	}
	return &ChunkCache{
		cap:       capBytes,
		chunkSize: chunkSize,
		items:     make(map[chunkKey]*lruEntry),
		lru:       list.New(),
	}
}

// ForLayer returns an io.ReaderAt that fetches compressed bytes for the
// given layer via underlying, populating the shared cache as it goes.
func (g *ChunkCache) ForLayer(layer v1.Hash, underlying io.ReaderAt, size int64) io.ReaderAt {
	return &layerView{global: g, layer: layer, underlying: underlying, size: size}
}

// Used reports the cache's current byte usage. Mostly for observability.
func (g *ChunkCache) Used() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used
}

func (g *ChunkCache) get(key chunkKey) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.items[key]
	if !ok {
		return nil, false
	}
	g.lru.MoveToFront(e.elem)
	return e.data, true
}

func (g *ChunkCache) put(key chunkKey, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, ok := g.items[key]; ok {
		// Race-loser or refresh: keep the existing entry.
		g.lru.MoveToFront(existing.elem)
		return
	}
	e := &lruEntry{key: key, data: data}
	e.elem = g.lru.PushFront(e)
	g.items[key] = e
	g.used += int64(len(data))

	if g.cap <= 0 {
		return
	}
	for g.used > g.cap {
		oldest := g.lru.Back()
		if oldest == nil {
			break
		}
		oe := oldest.Value.(*lruEntry)
		g.lru.Remove(oldest)
		delete(g.items, oe.key)
		g.used -= int64(len(oe.data))
	}
}

// layerView is the per-layer io.ReaderAt that fronts the global cache. It
// is the type that gsip actually consumes — a thin shim that translates
// (off, len) reads into chunk lookups + on-demand fetches.
type layerView struct {
	global     *ChunkCache
	layer      v1.Hash
	underlying io.ReaderAt
	size       int64
}

func (v *layerView) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= v.size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	if end > v.size {
		end = v.size
	}

	written := 0
	for cur := off; cur < end; {
		idx := cur / v.global.chunkSize
		chunkStart := idx * v.global.chunkSize
		chunkEnd := chunkStart + v.global.chunkSize
		if chunkEnd > v.size {
			chunkEnd = v.size
		}

		chunk, err := v.fetchChunk(idx, chunkStart, chunkEnd)
		if err != nil {
			return written, err
		}

		copyEnd := chunkEnd
		if copyEnd > end {
			copyEnd = end
		}
		n := copy(p[cur-off:], chunk[cur-chunkStart:copyEnd-chunkStart])
		written += n
		cur += int64(n)
		if n == 0 {
			break
		}
	}

	if int64(written) < int64(len(p)) {
		return written, io.EOF
	}
	return written, nil
}

func (v *layerView) fetchChunk(idx, start, end int64) ([]byte, error) {
	key := chunkKey{layer: v.layer, idx: idx}
	if data, ok := v.global.get(key); ok {
		return data, nil
	}
	buf := make([]byte, end-start)
	n, err := v.underlying.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	chunk := buf[:n]
	v.global.put(key, chunk)
	return chunk, nil
}
