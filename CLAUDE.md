# ocifuse

Read-only FUSE driver for browsing remote OCI images.

## The point

**Make individual files inside an OCI image readable as POSIX files, without ever pulling whole layers.** The whole reason this project exists, vs. just `crane export | tar xf`, is that you can `cat /mnt/.../bin/sh` and we fetch only the bytes needed for that one file via HTTP Range requests on the layer blob.

This is a load-bearing invariant. Optimizations are welcome — caching, prefetching, in-memory chunk caches, smarter checkpoint heuristics — but they must preserve partial-fetch semantics. **Do not propose "just download the whole layer" as a fix.** If a layer is 200MB and the user reads one 1KB file, we should fetch on the order of kilobytes, not megabytes.

The right shape of optimization: cache what we've already fetched (so repeat reads of overlapping ranges don't go back over the network), keep the indexes small and on disk, share parsed manifests across mounts. Not: defeat the partial-fetch design to win on benchmarks.

## Path layout

```
<mount>/<registry>/<repo...>/<ref>/<in-image-path>
```

`<ref>` is the first path segment containing `:` (tag) or `@` (digest). Everything before that names a registry plus repo path; everything after is in-image. Tag refs surface as symlinks pointing at the `repo@sha256:...` digest sibling. First segment without a `.` is Docker Hub shorthand.

## Architecture

- `internal/fspath` — parses FUSE-relative paths.
- `internal/oci` — go-containerregistry wrapper: resolve ref → manifest + config + layers + authenticated transport.
- `internal/layer` — per-layer indexer using `jonjohnsonjr/targz` (gsip + tarfs over ranger). Builds and persists gsip checkpoints + tarfs TOC keyed by layer digest. Layer blobs are *not* persisted; reads go through Range requests.
- `internal/merge` — folds N layers into one merged tree applying OCI whiteouts (`.wh.<name>`) and opaque markers (`.wh..wh..opq`). Synthesizes missing parent dirs.
- `internal/mount` — `hanwen/go-fuse/v2` NodeFS. Lazy `Lookup`-driven walk; tag→digest symlinks; merged trees mapped 1:1 to FUSE inodes.
- `cmd/ocifuse` — entrypoint. envconfig + clog. Knobs: `PLATFORM` (default `linux/amd64`), `CACHE_DIR` (default platform XDG), `CACHE_MAX_SIZE` (disk LRU cap, default `1GB`), `MEMORY_MAX_SIZE` (in-memory chunk cache LRU cap, default `1GB`), `TAG_TTL` (kernel dentry TTL on tag symlinks, default `1m`; bounds tag-update detection latency), `DEBUG`. Size knobs accept `K/M/G/T` suffixes; `0` disables eviction.

## Testing

- Unit tests: `internal/fspath`, `internal/merge`, `internal/mount` (pure-logic helpers only).
- `internal/mount/pipeline_test.go` runs the full resolve→index→merge→read pipeline against an in-process registry (`registry.New()` from go-containerregistry). Verifies byte-exact correctness via Range requests.
- `scripts/smoke-linux.sh` mounts the actual driver in a privileged Docker container and verifies file-content sha256 matches `crane export | tar -xO`. Use this when changing FUSE behavior — there is no way to reasonably test FUSE on darwin.
