# ocifuse

Read-only FUSE driver that mounts remote OCI images so individual files can be browsed with normal POSIX tools, fetching only the bytes needed via HTTP Range requests on the layer blobs.

```
$ ocifuse /mnt/oci &
$ cat /mnt/oci/index.docker.io/library/alpine:latest/etc/os-release
NAME="Alpine Linux"
ID=alpine
VERSION_ID=3.23.4
```

## Path layout

```
<mount>/<registry>/<repo...>/<ref>/<in-image-path>
```

`<ref>` is the first path segment containing `:` (a tag) or `@` (a digest). Tags surface as symlinks to their `repo@sha256:...` digest sibling. A leading segment without `.` is treated as Docker Hub shorthand. Multi-arch defaults to `linux/amd64`; override via `PLATFORM`.

## Usage

```
ocifuse <mountpoint>
```

Env vars:
- `PLATFORM` — default `linux/amd64`.
- `CACHE_DIR` — default `$XDG_CACHE_HOME/ocifuse`.
- `CACHE_MAX_SIZE` — disk LRU cap, default `1GB` (`K`/`M`/`G`/`T` suffixes; `0` disables).
- `MEMORY_MAX_SIZE` — in-memory chunk cache LRU cap, default `1GB`.
- `DEBUG` — verbose go-fuse logging.

Auth via go-containerregistry's `authn.DefaultKeychain` (`~/.docker/config.json`, gcloud, ECR, etc.).

## How it stays fast

Four layers of cache, all optional, all preserving the partial-fetch contract:

1. **Per-layer tar+gzip index** built with [`jonjohnsonjr/targz`](https://github.com/jonjohnsonjr/targz) (`gsip` checkpoints + `tarfs` TOC), persisted to disk by layer digest. Indexing streams the layer once; afterwards individual files decompress from the nearest checkpoint.
2. **Chunk cache** between gsip and HTTP. Range-fetched compressed bytes are remembered globally in an LRU; repeat reads of overlapping regions don't go back to the network.
3. **In-memory image cache** keyed by manifest digest, so repeat lookups skip the manifest fetch + merged-tree rebuild.
4. **Manifest/config disk cache** via a caching `http.RoundTripper` that intercepts only digest-keyed `manifests/sha256:...` and `blobs/sha256:...` responses (never layer blobs, never tags).

`singleflight.Group` deduplicates concurrent FUSE Lookups for the same ref.

The disk cache only ever holds indexes and small metadata — never layer blobs.

## Building and testing

```
go build ./cmd/ocifuse
go test ./...
```

For end-to-end FUSE without Linux hardware:

```
scripts/smoke-linux.sh    # asserts ocifuse-read sha256 matches `crane export`
scripts/docker-run.sh     # interactive shell inside the mounted container
```

macFUSE on darwin is theoretically supported but unreliable in practice; the container path is far smoother.

## Gotchas

- **Read-only.**
- **Tab completion needs a tweak.** Bash's default `COMP_WORDBREAKS` contains `:`, so `alpine:latest/etc/<TAB>` misfires. Fix:
  ```sh
  COMP_WORDBREAKS=${COMP_WORDBREAKS//:/}
  ```
  `scripts/docker-run.sh` does this inside the container.
- **Tab completion can't enumerate registries.** First time you type a registry/repo/tag segment it's all manual; after that the kernel caches what you've touched.

## Prior art

This project takes its central idea from [`jonjohnsonjr/dagdotdev`](https://github.com/jonjohnsonjr/dagdotdev), which uses the same `gsip` + `tarfs` + Range-request approach to serve OCI image content over HTTP. ocifuse rearranges those pieces behind a FUSE filesystem instead of a web UI.

See `CLAUDE.md` for the project contract on what optimizations are in-bounds, and `future-work.md` for what's been deferred.
