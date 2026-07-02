#!/usr/bin/env bash
set -euo pipefail

REPO_URL="https://github.com/a2899882/my-vps-probe.git"
BRANCH="${BRANCH:-main}"
INSTALL_DIR="${INSTALL_DIR:-/opt/my-vps-probe}"
GO_VERSION="${GO_VERSION:-1.24.5}"
SERVICE_NAME="probe-server"

echo "==> 1. install packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y git curl wget tar ca-certificates

if ! command -v go >/dev/null 2>&1; then
  echo "==> 2. install golang ${GO_VERSION}"
  cd /tmp
  rm -f "go${GO_VERSION}.linux-amd64.tar.gz"
  wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi

echo "==> 3. clone or update repo"
if [ ! -d "${INSTALL_DIR}/.git" ]; then
  rm -rf "${INSTALL_DIR}"
  git clone -b "${BRANCH}" --single-branch "${REPO_URL}" "${INSTALL_DIR}"
else
  git -C "${INSTALL_DIR}" fetch origin
  git -C "${INSTALL_DIR}" checkout "${BRANCH}"
  git -C "${INSTALL_DIR}" pull --ff-only origin "${BRANCH}"
fi

echo "==> 4. build server"
cd "${INSTALL_DIR}"
/usr/local/go/bin/go build -o "${INSTALL_DIR}/probe-server" ./server

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

[Install]
WantedBy=multi-user.target
EOF

echo "==> 6. enable service"
systemctl daemon-reload
systemctl enable ${SERVICE_NAME}
systemctl restart ${SERVICE_NAME}

echo "==> 7. status"
systemctl --no-pager --full status ${SERVICE_NAME} || true
echo
echo "Install done."
echo "Open: http://$(curl -s ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}'):8080"
