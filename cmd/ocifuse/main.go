// ocifuse mounts remote OCI images as a read-only FUSE filesystem.
//
// Usage:
//
//	ocifuse <mountpoint>
//
// Then files inside any OCI image become readable as
//
//	<mountpoint>/<registry>/<repo...>/<ref>/<in-image-path>
//
// where <ref> is a path segment containing ':' (tag) or '@' (digest), e.g.
//
//	cat <mountpoint>/index.docker.io/library/ubuntu:latest/etc/os-release
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/chainguard-dev/clog"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sethvargo/go-envconfig"

	"github.com/imjasonh/ocifuse/internal/cache"
	"github.com/imjasonh/ocifuse/internal/layer"
	"github.com/imjasonh/ocifuse/internal/mount"
	"github.com/imjasonh/ocifuse/internal/oci"
)

type config struct {
	Platform      string `env:"PLATFORM, default=linux/amd64"`
	CacheDir      string `env:"CACHE_DIR, default="`
	CacheMaxSize  string `env:"CACHE_MAX_SIZE, default=1GB"`  // disk; 0 disables
	MemoryMaxSize string `env:"MEMORY_MAX_SIZE, default=1GB"` // in-memory chunk cache; 0 disables
	Debug         bool   `env:"DEBUG, default=false"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ocifuse:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: %s <mountpoint>", os.Args[0])
	}
	mountpoint := os.Args[1]

	var cfg config
	if err := envconfig.Process(context.Background(), &cfg); err != nil {
		return fmt.Errorf("envconfig: %w", err)
	}

	platform, err := v1.ParsePlatform(cfg.Platform)
	if err != nil {
		return fmt.Errorf("parse PLATFORM=%q: %w", cfg.Platform, err)
	}

	cacheDir, err := resolveCacheDir(cfg.CacheDir)
	if err != nil {
		return err
	}
	maxBytes, err := parseBytes(cfg.CacheMaxSize)
	if err != nil {
		return fmt.Errorf("parse CACHE_MAX_SIZE=%q: %w", cfg.CacheMaxSize, err)
	}
	memBytes, err := parseBytes(cfg.MemoryMaxSize)
	if err != nil {
		return fmt.Errorf("parse MEMORY_MAX_SIZE=%q: %w", cfg.MemoryMaxSize, err)
	}
	c, err := cache.New(cacheDir, maxBytes)
	if err != nil {
		return err
	}
	oci.SetCache(c)

	log := clog.DefaultLogger()
	log.Info("ocifuse starting", "mountpoint", mountpoint, "platform", cfg.Platform, "cache_dir", cacheDir, "cache_max_bytes", maxBytes, "memory_max_bytes", memBytes)

	chunks := layer.NewChunkCache(memBytes, 1<<20)
	fsys := &mount.Filesystem{
		Platform: *platform,
		Indexer:  layer.NewIndexer(c, chunks),
	}

	server, err := fsys.Mount(mountpoint, cfg.Debug)
	if err != nil {
		return fmt.Errorf("mount %s: %w", mountpoint, err)
	}
	log.Info("mounted; press ctrl-c to unmount")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("unmounting")
		if err := server.Unmount(); err != nil {
			log.Error("unmount", "err", err)
		}
	}()

	server.Wait()
	return nil
}

// parseBytes accepts a human-readable size like "1GB", "500MB", "2.5GiB"
// (case-insensitive, with optional K/M/G/T suffix; "B" or no suffix = bytes).
// "0" or empty disables the cap.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("expected leading digits in %q", s)
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, err
	}
	suffix := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult int64 = 1
	switch suffix {
	case "", "B":
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size suffix %q", suffix)
	}
	return int64(num * float64(mult)), nil
}

func resolveCacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "ocifuse"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "ocifuse"), nil
}
