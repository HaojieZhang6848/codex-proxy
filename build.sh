#!/usr/bin/env bash
#
# Builds the release archives for Linux amd64 and Windows amd64 into dist/.
#
#   ./build.sh
#   VERSION=1.2.0 ./build.sh
#   TARGETS="linux/amd64" ./build.sh
#   SKIP_TESTS=1 ./build.sh
#
# Each archive contains the binary, config.example.yaml, README.md and the
# platform installer (install.sh for systemd, install.ps1 for the Windows
# service manager).
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

APP="codex-proxy"
MODULE="$(awk '/^module /{print $2; exit}' go.mod)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
STAGE_DIR="${DIST_DIR}/.stage"
TARGETS="${TARGETS:-linux/amd64 windows/amd64}"
SKIP_TESTS="${SKIP_TESTS:-0}"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

LDFLAGS="-s -w"
LDFLAGS="${LDFLAGS} -X ${MODULE}/internal/version.Version=${VERSION}"
LDFLAGS="${LDFLAGS} -X ${MODULE}/internal/version.Commit=${COMMIT}"
LDFLAGS="${LDFLAGS} -X ${MODULE}/internal/version.BuildTime=${BUILD_TIME}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "go is not installed"
[ -f "${ROOT_DIR}/go.mod" ] || die "go.mod not found in ${ROOT_DIR}"

if [ "${SKIP_TESTS}" != "1" ]; then
    log "go vet ./..."
    go vet ./...
    log "go test ./..."
    go test ./...
fi

rm -rf "${STAGE_DIR}"
mkdir -p "${STAGE_DIR}" "${DIST_DIR}"

check_sum() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$@"
    else
        shasum -a 256 "$@"
    fi
}

ARCHIVES=()

for target in ${TARGETS}; do
    GOOS="${target%%/*}"
    GOARCH="${target##*/}"
    case "${GOOS}" in
        linux|windows) ;;
        *) die "unsupported target ${target} (expected linux/amd64 or windows/amd64)" ;;
    esac
    case "${GOARCH}" in
        amd64) ;;
        *) warn "${target}: only amd64 is released, continuing because TARGETS asked for it" ;;
    esac

    EXT=""
    [ "${GOOS}" = "windows" ] && EXT=".exe"

    NAME="${APP}_${VERSION}_${GOOS}_${GOARCH}"
    STAGE="${STAGE_DIR}/${NAME}"
    mkdir -p "${STAGE}"

    log "building ${GOOS}/${GOARCH}"
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
        go build -trimpath -ldflags "${LDFLAGS}" -o "${STAGE}/${APP}${EXT}" "./cmd/${APP}"

    cp "${ROOT_DIR}/config.example.yaml" "${ROOT_DIR}/README.md" "${STAGE}/"
    case "${GOOS}" in
        linux)   install -m 0755 "${ROOT_DIR}/deploy/linux/install.sh" "${STAGE}/install.sh" ;;
        windows) cp "${ROOT_DIR}/deploy/windows/install.ps1" "${STAGE}/install.ps1" ;;
    esac

    ARCHIVE=""
    case "${GOOS}" in
        windows)
            command -v zip >/dev/null 2>&1 || die "zip is required to package the Windows release"
            ARCHIVE="${DIST_DIR}/${NAME}.zip"
            rm -f "${ARCHIVE}"
            ( cd "${STAGE_DIR}" && zip -q -r "${ARCHIVE}" "${NAME}" )
            ;;
        *)
            ARCHIVE="${DIST_DIR}/${NAME}.tar.gz"
            rm -f "${ARCHIVE}"
            tar -C "${STAGE_DIR}" -czf "${ARCHIVE}" "${NAME}"
            ;;
    esac
    log "packaged $(basename "${ARCHIVE}")"
    ARCHIVES+=("${ARCHIVE}")
done

rm -rf "${STAGE_DIR}"

log "writing SHA256SUMS"
( cd "${DIST_DIR}" && rm -f SHA256SUMS && check_sum ${ARCHIVES[*]##*/} > SHA256SUMS )

echo
log "release ${VERSION} (${COMMIT})"
for archive in "${ARCHIVES[@]}"; do
    printf '    %s (%s)\n' "$(basename "${archive}")" "$(du -h "${archive}" | cut -f1)"
done
printf '    %s\n' "$(basename "${DIST_DIR}")/SHA256SUMS"
