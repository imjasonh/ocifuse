#!/usr/bin/env bash
# Smoke-test ocifuse on Linux via a privileged Docker container.
#
# Cross-compiles for linux/$(uname -m), launches a container with /dev/fuse,
# mounts ocifuse, and asserts that:
#   - tag→digest symlinks resolve;
#   - Readdir, Stat, Readlink work;
#   - file bytes match the canonical layer (compared against `crane export`).
#
# Useful when developing on a machine without a working FUSE setup
# (e.g. macFUSE quirks on darwin). Requires docker.

set -euo pipefail

cd "$(dirname "$0")/.."

case "$(uname -m)" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 2 ;;
esac

BIN=/tmp/ocifuse-linux-${GOARCH}
echo "building ${BIN}"
GOOS=linux GOARCH=${GOARCH} go build -o "${BIN}" ./cmd/ocifuse/

docker run --rm --privileged --device /dev/fuse \
  -v "${BIN}:/usr/local/bin/ocifuse:ro" \
  alpine:latest \
  sh -c '
    set -e
    apk add --no-cache fuse fuse3 crane >/dev/null 2>&1
    mkdir -p /mnt/oci
    /usr/local/bin/ocifuse /mnt/oci &
    PID=$!
    trap "kill $PID 2>/dev/null; wait $PID 2>/dev/null" EXIT
    sleep 2

    REF=index.docker.io/library/alpine:latest
    BASE=/mnt/oci/${REF}

    echo "--- readdir root ---"
    ls "${BASE}/" >/dev/null

    echo "--- cat os-release ---"
    grep -q "^ID=alpine" "${BASE}/etc/os-release"

    echo "--- symlink target ---"
    test "$(readlink "${BASE}/bin/sh")" = "/bin/busybox"

    echo "--- byte-exact: busybox vs crane export ---"
    via_fuse=$(sha256sum "${BASE}/bin/busybox" | cut -d" " -f1)
    via_crane=$(crane export alpine:latest - | tar -xO bin/busybox | sha256sum | cut -d" " -f1)
    test "${via_fuse}" = "${via_crane}" || { echo "MISMATCH: ${via_fuse} vs ${via_crane}"; exit 1; }
    echo "ok: ${via_fuse}"

    echo "--- multi-layer (debian) ---"
    grep -q . /mnt/oci/index.docker.io/library/debian:stable-slim/etc/debian_version

    echo "--- tag symlink visible alongside digest sibling ---"
    ls -la /mnt/oci/index.docker.io/library/ | grep -q "alpine:latest -> alpine@sha256:"

    echo "ALL OK"
  '
