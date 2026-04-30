package layer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// countingRA is an io.ReaderAt that wraps a bytes.Reader and counts ReadAt calls.
type countingRA struct {
	r     io.ReaderAt
	calls atomic.Int64
}

func (c *countingRA) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

// hash returns a deterministic v1.Hash for tests.
func hash(s string) v1.Hash {
	sum := sha256.Sum256([]byte(s))
	return v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sum[:])}
}

func TestChunkCache_ServesFromMemoryAfterFirstFetch(t *testing.T) {
	data := make([]byte, 4*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	under := &countingRA{r: bytes.NewReader(data)}
	g := NewChunkCache(0, 1024)
	c := g.ForLayer(hash("layer"), under, int64(len(data)))

	got := make([]byte, 100)
	n, err := c.ReadAt(got, 50)
	if err != nil || n != 100 || !bytes.Equal(got, data[50:150]) {
		t.Fatalf("first read: n=%d err=%v match=%v", n, err, bytes.Equal(got, data[50:150]))
	}
	if c1 := under.calls.Load(); c1 != 1 {
		t.Errorf("after first read: underlying calls = %d, want 1", c1)
	}

	got2 := make([]byte, 50)
	if _, err := c.ReadAt(got2, 100); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, data[100:150]) {
		t.Errorf("second read mismatch")
	}
	if c2 := under.calls.Load(); c2 != 1 {
		t.Errorf("after second read: underlying calls = %d, want 1 (cache hit)", c2)
	}

	got3 := make([]byte, 1500)
	n, err = c.ReadAt(got3, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1500 || !bytes.Equal(got3, data[1500:3000]) {
		t.Errorf("third read: n=%d match=%v", n, bytes.Equal(got3, data[1500:3000]))
	}
	if c3 := under.calls.Load(); c3 != 3 {
		t.Errorf("after third read (spans 2 new chunks): underlying calls = %d, want 3", c3)
	}

	got4 := make([]byte, 1500)
	if _, err := c.ReadAt(got4, 1500); err != nil {
		t.Fatal(err)
	}
	if c4 := under.calls.Load(); c4 != 3 {
		t.Errorf("after re-read: underlying calls = %d, want 3 (full cache hit)", c4)
	}
}

func TestChunkCache_EOFAtEnd(t *testing.T) {
	data := []byte("hello world")
	under := &countingRA{r: bytes.NewReader(data)}
	g := NewChunkCache(0, 4)
	c := g.ForLayer(hash("layer"), under, int64(len(data)))

	got := make([]byte, len(data))
	n, err := c.ReadAt(got, 0)
	if n != len(data) || err != nil {
		t.Errorf("exact-fit: n=%d err=%v want %d,nil", n, err, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch")
	}

	got2 := make([]byte, 5)
	n, err = c.ReadAt(got2, int64(len(data)))
	if n != 0 || err != io.EOF {
		t.Errorf("read past end: n=%d err=%v want 0,EOF", n, err)
	}

	got3 := make([]byte, 10)
	n, err = c.ReadAt(got3, 5)
	if n != 6 || err != io.EOF {
		t.Errorf("straddle: n=%d err=%v want 6,EOF", n, err)
	}
	if !bytes.Equal(got3[:n], data[5:]) {
		t.Errorf("straddle content mismatch")
	}
}

func TestChunkCache_EvictsLRUWhenOverCap(t *testing.T) {
	// Two layers, each 4KB, in 1KB chunks. Cap = 4KB total — fits 4 chunks.
	// We'll fetch all of layer A (4 chunks), then all of layer B (4 chunks),
	// pushing A out. Re-reading A should re-fetch from underlying.
	mkData := func(b byte) []byte {
		out := make([]byte, 4096)
		for i := range out {
			out[i] = b
		}
		return out
	}
	dataA := mkData('A')
	dataB := mkData('B')
	underA := &countingRA{r: bytes.NewReader(dataA)}
	underB := &countingRA{r: bytes.NewReader(dataB)}
	g := NewChunkCache(4096, 1024) // 4KB cap

	cA := g.ForLayer(hash("A"), underA, int64(len(dataA)))
	cB := g.ForLayer(hash("B"), underB, int64(len(dataB)))

	// Fill cache with all of A. 4 chunks fetched, 4KB used.
	if _, err := cA.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if got := g.Used(); got != 4096 {
		t.Errorf("Used = %d, want 4096", got)
	}
	if got := underA.calls.Load(); got != 4 {
		t.Errorf("A calls = %d, want 4", got)
	}

	// Fill cache with all of B. Each B chunk pushes an A chunk out.
	if _, err := cB.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if got := g.Used(); got != 4096 {
		t.Errorf("Used = %d, want 4096 (cap held)", got)
	}
	if got := underB.calls.Load(); got != 4 {
		t.Errorf("B calls = %d, want 4", got)
	}

	// Re-read A: every chunk is gone, must re-fetch.
	if _, err := cA.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if got := underA.calls.Load(); got != 8 {
		t.Errorf("A calls after eviction = %d, want 8", got)
	}
}
