# Future work

### Medium-term

- **Tag listing** — populate repo directories from the registry catalog API where the registry permits.
- **Prefetch heuristics** — read-ahead within a layer for sequential access; speculative fetch of neighboring files in the same tar.
- **Content-addressed dedup** — share file content across images that reuse identical blobs and offsets.
- **macFUSE polish** — once Linux is solid, exercise on darwin, document setup, work around any kext quirks.
- **Tag→digest disk persistence with TTL** — fresh-mount tag resolution still costs ~200ms per ref (correctly going to network because tags move). A short-TTL on-disk record could amortize across restarts within a window without sacrificing correctness.

### Far Future
- **Write support** — overlay / copy-on-write semantics on top of the read-only base.
- **Local OCI layout sources** — read from on-disk OCI layouts and Docker daemon, not just remote registries.
