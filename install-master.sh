#!/usr/bin/env bash
set -euo pipefail

REPO_URL="https://github.com/a2899882/my-vps-probe.git"
BRANCH="${BRANCH:-main}"
INSTALL_DIR="${INSTALL_DIR:-/opt/my-vps-probe}"
GO_VERSION="${GO_VERSION:-1.25.0}"
SERVICE_NAME="probe-server"

echo "==> 1. install packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y git curl wget tar ca-certificates build-essential

install_go=true
if command -v go >/dev/null 2>&1; then
  current_go_version="$(go version 2>/dev/null | awk '{sub(/^go/, "", $3); print $3}')"
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
else
  echo "==> 2. golang ${current_go_version} is ready"
fi

echo "==> 3. clone or update repo"
if [ ! -d "${INSTALL_DIR}/.git" ]; then
  rm -rf "${INSTALL_DIR}"
  git clone -b "${BRANCH}" --single-branch --depth 1 "${REPO_URL}" "${INSTALL_DIR}"
else
  git -C "${INSTALL_DIR}" restore --worktree --staged \
    probe-server probe-agent-amd64 probe-agent-arm64 \
    server/probe-agent-amd64 server/probe-agent-arm64 \
    go1.22.4.linux-amd64.tar.gz 2>/dev/null || true
  git -C "${INSTALL_DIR}" fetch origin
  git -C "${INSTALL_DIR}" checkout "${BRANCH}"
  git -C "${INSTALL_DIR}" pull --ff-only origin "${BRANCH}"
fi

echo "==> 4. build server and Linux agents"
cd "${INSTALL_DIR}"
GO_BIN="/usr/local/go/bin/go"
[ -x "$GO_BIN" ] || GO_BIN="$(command -v go)"

"$GO_BIN" build -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/.probe-server.new" ./server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/server/.probe-agent-amd64.new" ./agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO_BIN" build -trimpath -ldflags="-s -w" -o "${INSTALL_DIR}/server/.probe-agent-arm64.new" ./agent
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
systemctl --no-pager --full status ${SERVICE_NAME} || true
echo
echo "Install done."
echo "Open: http://$(curl -s ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}'):8080"
