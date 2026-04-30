#!/usr/bin/env bash
# Drop into an interactive shell inside a privileged Linux container with
# ocifuse already mounted at /mnt/oci.
#
# Useful for poking at OCI images with normal POSIX tools — ls, cat, find,
# stat, sha256sum — when the host doesn't have a working FUSE setup
# (e.g. darwin without macFUSE, or with macFUSE behaving oddly).
#
# Cross-compiles for the host arch, mounts the binary into an alpine
# container with /dev/fuse, and starts ocifuse in the background. On exit
# the container is torn down.

set -euo pipefail
cd "$(dirname "$0")/.."

case "$(uname -m)" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 2 ;;
esac

BIN=/tmp/ocifuse-linux-${GOARCH}
echo "building ${BIN}..."
GOOS=linux GOARCH=${GOARCH} go build -o "${BIN}" ./cmd/ocifuse/

PLATFORM="${PLATFORM:-linux/amd64}"
CACHE_MAX_SIZE="${CACHE_MAX_SIZE:-1GB}"

docker run --rm -it --privileged --device /dev/fuse \
  -v "${BIN}:/usr/local/bin/ocifuse:ro" \
  -e PLATFORM="${PLATFORM}" \
  -e CACHE_MAX_SIZE="${CACHE_MAX_SIZE}" \
  alpine:latest \
  sh -c '
    apk add --no-cache fuse fuse3 bash bash-completion >/dev/null 2>&1
    mkdir -p /mnt/oci
    /usr/local/bin/ocifuse /mnt/oci > /var/log/ocifuse.log 2>&1 &
    PID=$!
    trap "kill $PID 2>/dev/null; wait $PID 2>/dev/null" EXIT
    sleep 1.5

    # Bash treats `:` as a word-break for completion, which breaks tab on
    # OCI paths like `alpine:latest`. Drop it so completion sees full
    # segments. Done in /root/.bashrc since COMP_WORDBREAKS is bash-special
    # and is reset on shell start.
    cat > /root/.bashrc <<RC
COMP_WORDBREAKS=\${COMP_WORDBREAKS//:/}
PS1="ocifuse:\w\\\$ "
cd /mnt/oci 2>/dev/null
RC

    cat <<EOF

ocifuse is mounted at /mnt/oci (PLATFORM=${PLATFORM}, CACHE_MAX_SIZE=${CACHE_MAX_SIZE}).
Logs: tail -f /var/log/ocifuse.log

Try:
  ls index.docker.io/library/alpine:latest/etc/
  cat index.docker.io/library/alpine:latest/etc/os-release
  ls -la index.docker.io/library/  # see the tag symlink

Tab completion works on segments you have already touched (the kernel
caches them) and on paths inside a resolved image. It cannot enumerate
registries or repos — those are not listable via the registry API.

Exit to tear the container down.

EOF
    exec bash -i
  '
