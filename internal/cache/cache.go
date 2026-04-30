// Package cache is a tiny disk-backed key/value store rooted at a directory,
// with optional LRU eviction once total size exceeds a configured cap. Keys
// are slash-separated paths (e.g. "layers/sha256/abc.../tarfs.toc"); values
// are byte streams.
//
// LRU ordering uses filesystem mtime: every Open() touches the file's
// mtime to "now", and eviction (triggered after writes that put us over cap)
// removes files in ascending mtime order until total size is under cap.
package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Cache struct {
	dir string
	cap int64 // 0 disables eviction

	mu   sync.Mutex
	size int64 // tracked total bytes; valid when cap > 0
}

// New opens (or creates) a cache rooted at dir. If maxBytes > 0, total disk
// usage is held under that cap by LRU eviction; 0 disables eviction.
//
// When eviction is enabled, New walks the directory once to seed the size
// counter — usually fast for our cache (a few hundred small files).
func New(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	c := &Cache{dir: dir, cap: maxBytes}
	if maxBytes > 0 {
		if err := c.rescan(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// rescan walks the cache dir and resets c.size to the total bytes on disk.
func (c *Cache) rescan() error {
	var total int64
	err := filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.size = total
	c.mu.Unlock()
	return nil
}

// Path returns the absolute filesystem path for a cache key.
func (c *Cache) Path(key string) string {
	return filepath.Join(c.dir, filepath.FromSlash(key))
}

// Open opens a cached value for reading. Returns os.ErrNotExist if absent.
// Successful opens touch the file's mtime so the LRU sees it as recent.
func (c *Cache) Open(key string) (io.ReadCloser, error) {
	p := c.Path(key)
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return f, nil
}

// Has reports whether key exists.
func (c *Cache) Has(key string) bool {
	_, err := os.Stat(c.Path(key))
	return err == nil
}

// Write atomically stores f's output under key, then triggers LRU eviction
// if the cache has a cap and is now over it.
func (c *Cache) Write(key string, f func(io.Writer) error) error {
	p := c.Path(key)
	var oldSize int64
	if info, err := os.Stat(p); err == nil {
		oldSize = info.Size()
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := f(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}

	if c.cap <= 0 {
		return nil
	}
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	newSize := info.Size()

	c.mu.Lock()
	c.size += newSize - oldSize
	over := c.size > c.cap
	c.mu.Unlock()

	if over {
		c.evict()
	}
	return nil
}

// evict deletes least-recently-used files (ascending mtime) until total size
// is under cap. Best effort; errors are silently skipped because losing a
// cache entry is recoverable.
func (c *Cache) evict() {
	type entry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var entries []entry
	_ = filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		entries = append(entries, entry{path, info.Size(), info.ModTime()})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		if c.size <= c.cap {
			break
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		c.size -= e.size
		// Best-effort: prune now-empty parent dir.
		_ = os.Remove(filepath.Dir(e.path))
	}
}

// Size reports the cache's currently-tracked total byte usage. Lazily
// initialized on first Write.
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}
