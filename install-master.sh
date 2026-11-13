#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/a2899882/my-vps-probe.git}"
BRANCH="${BRANCH:-main}"
INSTALL_DIR="${INSTALL_DIR:-/opt/my-vps-probe}"
GO_VERSION="${GO_VERSION:-1.25.0}"
SERVICE_NAME="probe-server"

SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

echo "==> 1. install packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git curl wget tar unzip ca-certificates build-essential python3 sqlite3

install_go=true
if command -v go >/dev/null 2>&1 || [ -x /usr/local/go/bin/go ]; then
  GO_CHECK="/usr/local/go/bin/go"
  [ -x "$GO_CHECK" ] || GO_CHECK="$(command -v go)"
  current_go_version="$("$GO_CHECK" version 2>/dev/null | awk '{sub(/^go/, "", $3); print $3}')"
  if [ -n "$current_go_version" ] &&
     [ "$(printf '%s\n' "$GO_VERSION" "$current_go_version" | sort -V | head -n 1)" = "$GO_VERSION" ]; then
    install_go=false
  fi
fi

if $install_go; then
  echo "==> 2. install/upgrade golang ${GO_VERSION}"
  cd /tmp
  machine_arch="$(uname -m)"
  case "$machine_arch" in
    x86_64|amd64) go_arch="amd64" ;;
    aarch64|arm64) go_arch="arm64" ;;
    *) echo "Unsupported server architecture: $machine_arch" >&2; exit 1 ;;
  esac
  archive="go${GO_VERSION}.linux-${go_arch}.tar.gz"
  rm -f "$archive"
  wget -q "https://go.dev/dl/${archive}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$archive"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  rm -f "$archive"
else
  echo "==> 2. golang ${current_go_version} is ready"
fi

echo "==> 3. prepare source in ${INSTALL_DIR} (preserving existing data)"
mkdir -p "${INSTALL_DIR}"

# Preserve existing runtime data if present
PRESERVE_DIR="$(mktemp -d)"
trap 'rm -rf "$PRESERVE_DIR"' EXIT
for f in config.json data.db data.db-wal data.db-shm usage_state.json; do
  if [ -f "${INSTALL_DIR}/$f" ]; then
    cp -pf "${INSTALL_DIR}/$f" "${PRESERVE_DIR}/$f"
  fi
done

if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/server/main.go" ] && [ -f "$SCRIPT_DIR/agent/main.go" ]; then
  echo "    Detected local source directory: ${SCRIPT_DIR}"
  if [ "$SCRIPT_DIR" != "$INSTALL_DIR" ]; then
    mkdir -p "${INSTALL_DIR}/server" "${INSTALL_DIR}/agent" "${INSTALL_DIR}/common" "${INSTALL_DIR}/tcpping" "${INSTALL_DIR}/scripts" "${INSTALL_DIR}/tests"
    cp -rf "$SCRIPT_DIR/server/"*.go "$SCRIPT_DIR/server/"*.html "${INSTALL_DIR}/server/"
    cp -rf "$SCRIPT_DIR/agent/"* "${INSTALL_DIR}/agent/"
    cp -rf "$SCRIPT_DIR/common/"* "${INSTALL_DIR}/common/"
    cp -rf "$SCRIPT_DIR/tcpping/"* "${INSTALL_DIR}/tcpping/"
    cp -rf "$SCRIPT_DIR/scripts/"* "${INSTALL_DIR}/scripts/"
    [ -d "$SCRIPT_DIR/tests" ] && cp -rf "$SCRIPT_DIR/tests/"* "${INSTALL_DIR}/tests/" 2>/dev/null || true
    for f in go.mod go.sum install.sh install-master.sh uninstall-agent.sh README.md; do
      [ -f "$SCRIPT_DIR/$f" ] && cp -f "$SCRIPT_DIR/$f" "${INSTALL_DIR}/$f"
    done
  fi
elif [ ! -d "${INSTALL_DIR}/.git" ]; then
  tmp_clone="$(mktemp -d)"
  git clone -b "${BRANCH}" --single-branch --depth 1 "${REPO_URL}" "$tmp_clone"
  cp -a "$tmp_clone/." "${INSTALL_DIR}/"
  rm -rf "$tmp_clone"
else
  git -C "${INSTALL_DIR}" restore --worktree --staged \
    probe-server probe-agent-amd64 probe-agent-arm64 \
    server/probe-agent-amd64 server/probe-agent-arm64 \
    go1.22.4.linux-amd64.tar.gz 2>/dev/null || true
  git -C "${INSTALL_DIR}" fetch origin
  git -C "${INSTALL_DIR}" checkout "${BRANCH}"
  git -C "${INSTALL_DIR}" pull --ff-only origin "${BRANCH}"
fi

# Restore any preserved data files
for f in config.json data.db data.db-wal data.db-shm usage_state.json; do
  if [ -f "${PRESERVE_DIR}/$f" ] && [ ! -f "${INSTALL_DIR}/$f" ]; then
    cp -pf "${PRESERVE_DIR}/$f" "${INSTALL_DIR}/$f"
  fi
done

echo "==> 4. build server and Linux agents (low-RAM safe mode)"
cd "${INSTALL_DIR}"
GO_BIN="/usr/local/go/bin/go"
[ -x "$GO_BIN" ] || GO_BIN="$(command -v go)"

env GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/.probe-server.new" ./server
env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/server/.probe-agent-amd64.new" ./agent
env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/server/.probe-agent-arm64.new" ./agent
install -m 0755 "${INSTALL_DIR}/.probe-server.new" "${INSTALL_DIR}/probe-server"
install -m 0755 "${INSTALL_DIR}/server/.probe-agent-amd64.new" "${INSTALL_DIR}/server/probe-agent-amd64"
install -m 0755 "${INSTALL_DIR}/server/.probe-agent-arm64.new" "${INSTALL_DIR}/server/probe-agent-arm64"
rm -f "${INSTALL_DIR}/.probe-server.new" "${INSTALL_DIR}/server/.probe-agent-amd64.new" "${INSTALL_DIR}/server/.probe-agent-arm64.new"
chmod 0755 \
  "${INSTALL_DIR}/probe-server" \
  "${INSTALL_DIR}/server/probe-agent-amd64" \
  "${INSTALL_DIR}/server/probe-agent-arm64"

echo "==> 5. install systemd service"
cat > /etc/systemd/system/${SERVICE_NAME}.service <<EOF
[Unit]
Description=My VPS Probe Server
After=network.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
Environment="GOGC=75"
Environment="GOMEMLIMIT=64MiB"
ExecStart=${INSTALL_DIR}/probe-server
Restart=always
RestartSec=3
User=root
NoNewPrivileges=true
PrivateTmp=true
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

echo "==> 5.5 install tz menu"
install -m 755 "${INSTALL_DIR}/scripts/tz.sh" /usr/local/bin/tz
ln -sf /usr/local/bin/tz /usr/bin/tz 2>/dev/null || true

echo "==> 6. enable service"
systemctl daemon-reload
systemctl enable ${SERVICE_NAME}
systemctl restart ${SERVICE_NAME}
for _ in {1..20}; do
  curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://127.0.0.1:8080/healthz >/dev/null

echo "==> 7. status"
systemctl --no-pager -n 10 status ${SERVICE_NAME} || true
echo
echo "Install/Update done."
echo "Run 'tz' in SSH to open the management menu."
echo "Open: http://$(curl -s --max-time 4 ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}'):8080"
