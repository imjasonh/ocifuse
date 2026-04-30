# ocifuse — read-only FUSE for OCI images

Mount remote OCI images so files are browsable with normal POSIX tools, fetching only the bytes actually needed via HTTP Range requests on layer blobs. Linux first; macFUSE smoke later.

## Path layout

```
<mount>/<registry>/<repo...>/<ref>/<in-image-path>
```

- `<ref>` is the path segment containing `:` (tag) or `@` (digest). That segment terminates the ref; everything after is in-image.
- Tags surface as symlinks to the resolved `@sha256:...` sibling.
- First segment with no `.` is Docker Hub shorthand → `index.docker.io/library/<name>`.
- Multi-arch defaults to `linux/amd64`; override via `--platform`.

Examples:
- `/mnt/index.docker.io/library/ubuntu:latest/etc/os-release`
- `/mnt/gcr.io/distroless/base@sha256:abc.../usr/bin/sh`
- `/mnt/ubuntu:22.04/etc/os-release` (shorthand)

## Components

- **`cmd/ocifuse`** — entrypoint. Flags + envconfig for `<mountpoint>`, `--platform`, `--cache-dir`. clog for logging.
- **`internal/mount`** — `hanwen/go-fuse/v2` NodeFS. Read-only. Long entry/attr TTL for digest-pinned nodes (immutable); short TTL for tags. (Named `mount` rather than `fuse` to avoid clashing with the `go-fuse/v2/fuse` import.)
- **`internal/oci`** — go-containerregistry wrapper. `Resolve(ref)` → manifest + config + layer descriptors. `authn.DefaultKeychain`. Platform selection from index manifests.
- **`internal/layer`** — `jonjohnsonjr/targz` (tarfs/ranger/gsip). `Open` streams the layer blob once via `tarfs.Index` to build a gzip-checkpointed index plus tar TOC, both persisted by layer digest. Subsequent reads issue Range requests for just the bytes a file actually needs. Layer blobs are not persisted, only indexes.

  **`third_party/targz`** vendors targz (via `replace` in go.mod) with two upstream-bug fixes: `gsip.Reader.ReadAt` discarded partial bytes on short read at EOF (now returns `(n, io.EOF)`); `acquireReader` mis-passed total size as section length, letting Range requests spill past blob end.
- **`internal/merge`** — folds N layer indexes into one merged tree applying OCI whiteouts (`.wh.<name>`) and opaque markers (`.wh..wh..opq`), bottom-up. Built once per image digest, cached on disk. Each merged entry records `(name, mode, size, layer-of-origin, in-layer-offset)`.
- **`internal/cache`** — disk cache at `$XDG_CACHE_HOME/ocifuse` (platform default fallback). Stores manifests, configs, layer indexes, merged trees, all keyed by digest (immutable). Tag→digest resolutions cached separately with short TTL.

## Phases

1. **Path parser** — parse our layout per the rules above. Built on `name.ParseReference`. Unit tests for shorthand, multi-segment repos, tag-vs-digest detection.
2. **OCI fetch** — fetch+cache manifest, config, layer descriptors for a known digest. No FUSE yet; CLI subcommand for inspection.
3. **Layer indexing** — wire targz/ranger; build+persist an index for one real layer; range-read a single file. Standalone test binary.
4. **Merge** — fold N layers into one tree with whiteout/opaque handling. Test against busybox, distroless, ubuntu fixtures.
5. **FUSE mount** — minimal mount serving one hardcoded `<registry>/<repo>@<digest>/...`. Verify with `cat`, `ls`, `find`, `stat`, `readlink`.
6. **Lazy resolution** — generic path-walk that resolves refs on demand; registry/repo intermediate dirs are lazy.
7. **Tags as symlinks** — tag segment reports as symlink to digest sibling; tag→digest revalidation on TTL expiry.
8. **Polish** — `--platform`, auth surfacing, ENOENT vs EIO discipline, structured logging via clog.

## Testing

- Unit tests: path parser, whiteout merge.
- Integration: `rogpeppe/go-internal/testscript`. Spin up an in-process registry (go-containerregistry's `registry` package), push known fixtures, mount, exercise via shell.
- Manual smoke: ubuntu, distroless, busybox, then a multi-arch image to exercise platform selection.

## Out of scope

See `future-work.md`.
