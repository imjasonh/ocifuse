# Future work

### Soon

- **Per-ref platform selection** — encode platform in the ref segment (e.g. `<ref>~linux-arm64`) so a single mount can serve multiple architectures.
- **Injected `meta` directory** -- add a directory `@@meta` (or something) that includes `./layers/<sha256:...>` for each layer in the image, and `digest` that is the image's digest so you can `cat /mnt/oci/<ref:tag(~platform)>/@@meta/digest` to get the image's digest. `./layers` directories also have a `@@meta/layer-digest` for that layer's digest.
- **Per-layer view** — `<ref>/layers/<layer-digest>/...` exposing each raw layer alongside the merged view, for debugging overlays and whiteouts.

### Medium-term

- **Tag listing** — populate repo directories from the registry catalog API where the registry permits.
- **Prefetch heuristics** — read-ahead within a layer for sequential access; speculative fetch of neighboring files in the same tar.
- **Content-addressed dedup** — share file content across images that reuse identical blobs and offsets.
- **macFUSE polish** — once Linux is solid, exercise on darwin, document setup, work around any kext quirks.
- **Tag→digest disk persistence with TTL** — fresh-mount tag resolution still costs ~200ms per ref (correctly going to network because tags move). A short-TTL on-disk record could amortize across restarts within a window without sacrificing correctness.

### Far Future
- **Write support** — overlay / copy-on-write semantics on top of the read-only base.
- **Local OCI layout sources** — read from on-disk OCI layouts and Docker daemon, not just remote registries.
