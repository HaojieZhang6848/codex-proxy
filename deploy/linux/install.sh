#!/usr/bin/env bash
#
# Installs codex-proxy as a systemd service.
#
#   sudo ./install.sh              install or upgrade
#   sudo ./install.sh --uninstall  remove the service (keeps config and secrets)
#   sudo ./install.sh --uninstall --purge
#                                  also delete configuration, state and logs
#
# The script is meant to be run from the unpacked release directory.
#
set -euo pipefail

APP="codex-proxy"
SERVICE="codex-proxy"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/codex-proxy}"
STATE_DIR="${STATE_DIR:-/var/lib/codex-proxy}"
LOG_DIR="${LOG_DIR:-/var/log/codex-proxy}"
SERVICE_USER="${SERVICE_USER:-codex-proxy}"
UNIT_PATH="/etc/systemd/system/${SERVICE}.service"
SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

UNINSTALL=0
PURGE=0

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    cat <<'EOF'

Environment overrides:
  BIN_DIR, CONFIG_DIR, STATE_DIR, LOG_DIR, SERVICE_USER
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --uninstall) UNINSTALL=1 ;;
        --purge)     PURGE=1 ;;
        -h|--help)   usage; exit 0 ;;
        *)           die "unknown argument: $1" ;;
    esac
    shift
done

[ "$(id -u)" -eq 0 ] || die "run this script as root: sudo $0 $*"
command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"

uninstall() {
    log "stopping ${SERVICE}"
    systemctl stop "${SERVICE}" 2>/dev/null || true
    systemctl disable "${SERVICE}" 2>/dev/null || true
    if [ -f "${UNIT_PATH}" ]; then
        rm -f "${UNIT_PATH}"
        systemctl daemon-reload
    fi
    systemctl reset-failed "${SERVICE}" 2>/dev/null || true
    rm -f "${BIN_DIR}/${APP}"

    if [ "${PURGE}" -eq 1 ]; then
        log "removing configuration, state and logs"
        rm -rf "${CONFIG_DIR}" "${STATE_DIR}" "${LOG_DIR}"
        if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
            userdel "${SERVICE_USER}" 2>/dev/null || warn "could not remove the ${SERVICE_USER} user"
        fi
    fi
    if [ "${PURGE}" -eq 1 ]; then
        log "uninstalled and purged"
    else
        log "uninstalled; configuration in ${CONFIG_DIR} was kept (use --purge to delete it)"
    fi
}

install_service() {
    [ -f "${SOURCE_DIR}/${APP}" ] || die "${APP} binary not found next to this script; run it from the unpacked release directory"

    log "installing ${BIN_DIR}/${APP}"
    install -d -m 0755 "${BIN_DIR}"
    install -m 0755 "${SOURCE_DIR}/${APP}" "${BIN_DIR}/${APP}"

    if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        log "creating the ${SERVICE_USER} system user"
        if command -v useradd >/dev/null 2>&1; then
            useradd --system --no-create-home --shell "$(command -v nologin || echo /usr/sbin/nologin)" "${SERVICE_USER}"
        elif command -v adduser >/dev/null 2>&1; then
            adduser --system --no-create-home --group "${SERVICE_USER}"
        else
            die "cannot create the ${SERVICE_USER} user: neither useradd nor adduser is available"
        fi
    fi

    install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${CONFIG_DIR}"
    install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${STATE_DIR}"
    install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${LOG_DIR}"

    if [ -f "${CONFIG_DIR}/config.yaml" ]; then
        log "keeping the existing ${CONFIG_DIR}/config.yaml"
    else
        log "installing ${CONFIG_DIR}/config.yaml (from config.example.yaml)"
        install -m 0640 -o root -g "${SERVICE_USER}" "${SOURCE_DIR}/config.example.yaml" "${CONFIG_DIR}/config.yaml"
        warn "edit ${CONFIG_DIR}/config.yaml before the proxy can serve requests: it still contains ${VAR} placeholders"
    fi

    if [ ! -f "${CONFIG_DIR}/env" ]; then
        install -m 0640 -o root -g "${SERVICE_USER}" /dev/null "${CONFIG_DIR}/env"
    fi

    log "writing ${UNIT_PATH}"
    cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=codex-proxy (Codex Responses API forwarder)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${STATE_DIR}
ExecStart=${BIN_DIR}/${APP} run -config ${CONFIG_DIR}/config.yaml
# Optional environment file: put HTTP_PROXY / HTTPS_PROXY / NO_PROXY or any
# \${VAR} used by the configuration file in there.
EnvironmentFile=-${CONFIG_DIR}/env
Restart=on-failure
RestartSec=2s

# Hardening. ProtectSystem=strict makes the file system read only, so the
# directories below are the only places the service may write to.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true
ReadWritePaths=${LOG_DIR} ${STATE_DIR}
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    log "enabling and starting ${SERVICE}"
    systemctl enable "${SERVICE}" >/dev/null
    systemctl restart "${SERVICE}"

    sleep 1
    if systemctl is-active --quiet "${SERVICE}"; then
        log "${SERVICE} is running"
    else
        warn "${SERVICE} is not running; check: journalctl -u ${SERVICE} -n 50"
    fi
    echo
    log "configuration: ${CONFIG_DIR}/config.yaml"
    log "logs:          journalctl -u ${SERVICE} -f"
    log "status:        systemctl status ${SERVICE}"
}

if [ "${UNINSTALL}" -eq 1 ]; then
    uninstall
else
    install_service
fi
