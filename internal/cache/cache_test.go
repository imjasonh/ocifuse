package cache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKey(t *testing.T, c *Cache, key string, body []byte) {
	t.Helper()
	if err := c.Write(key, func(w io.Writer) error { _, err := w.Write(body); return err }); err != nil {
		t.Fatal(err)
	}
}

func readKey(t *testing.T, c *Cache, key string) []byte {
	t.Helper()
	r, err := c.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCache_NoEvictionWhenUnderCap(t *testing.T) {
	c, err := New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, c, "a", bytes.Repeat([]byte{'x'}, 100))
	writeKey(t, c, "b", bytes.Repeat([]byte{'y'}, 100))
	if !c.Has("a") || !c.Has("b") {
		t.Errorf("entries should not have been evicted")
	}
}

func TestCache_EvictsLRUWhenOverCap(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, 250)
	if err != nil {
		t.Fatal(err)
	}
	// Write three 100-byte entries. Cap is 250 → after the third, total is
	// 300 > 250, so the oldest should evict.
	writeKey(t, c, "old", bytes.Repeat([]byte{'A'}, 100))
	// Force ascending mtime so "old" is unambiguously oldest.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old"), past, past); err != nil {
		t.Fatal(err)
	}
	writeKey(t, c, "mid", bytes.Repeat([]byte{'B'}, 100))
	writeKey(t, c, "new", bytes.Repeat([]byte{'C'}, 100))

	if c.Has("old") {
		t.Errorf("oldest entry should have been evicted")
	}
	if !c.Has("mid") || !c.Has("new") {
		t.Errorf("newer entries should survive")
	}
	if got := c.Size(); got > 250 {
		t.Errorf("size = %d, expected <= cap 250", got)
	}
}

func TestCache_ReadTouchesMtime(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, c, "a", []byte("hello"))

	past := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(c.Path("a"), past, past); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(c.Path("a"))

	_ = readKey(t, c, "a")
	after, _ := os.Stat(c.Path("a"))

	if !after.ModTime().After(before.ModTime()) {
		t.Errorf("Open should have touched mtime: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

func TestCache_NoEvictionWhenCapZero(t *testing.T) {
	c, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeKey(t, c, string(rune('a'+i)), bytes.Repeat([]byte{'X'}, 1000))
	}
	for i := 0; i < 5; i++ {
		if !c.Has(string(rune('a' + i))) {
			t.Errorf("entry %c missing — eviction shouldn't fire when cap=0", 'a'+i)
		}
	}
}
