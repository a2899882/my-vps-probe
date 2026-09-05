#!/bin/sh
set -eu

SERVER=""
TOKEN=""

while getopts "s:t:" opt; do
  case "$opt" in
    s) SERVER=$OPTARG ;;
    t) TOKEN=$OPTARG ;;
    *) exit 2 ;;
  esac
done

fail() {
  echo "❌ $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "请使用 root 用户执行部署命令"
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || fail "缺少主控地址或节点 Token"

case "$SERVER$TOKEN" in
  *"
"*) fail "主控地址或 Token 不能包含换行符" ;;
esac

case "$SERVER" in
  http://*|https://*) BASE_URL=${SERVER%/} ;;
  *) BASE_URL="https://${SERVER%/}" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) AGENT_ARCH=amd64 ;;
  aarch64|arm64) AGENT_ARCH=arm64 ;;
  *) fail "暂不支持的架构: $(uname -m)（支持 amd64 / arm64）" ;;
esac

detect_os() {
  if [ -f /etc/alpine-release ]; then
    echo alpine
  elif [ -f /etc/debian_version ]; then
    echo debian
  elif [ -f /etc/redhat-release ]; then
    echo redhat
  else
    echo linux
  fi
}

ensure_downloader() {
  if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
    return
  fi
  if command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl >/dev/null
  elif command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl
  else
    fail "系统缺少 curl/wget，且未识别到可用的软件包管理器"
  fi
}

download_file() {
  url=$1
  output=$2
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --retry-delay 1 --connect-timeout 10 --max-time 90 "$url" -o "$output"
  else
    wget -q -T 90 -O "$output" "$url"
  fi
}

shell_quote() {
  printf "'"
  printf '%s' "$1" | sed "s/'/'\\\\''/g"
  printf "'"
}

OS_NAME=$(detect_os)
DL_URL="${BASE_URL}/probe-agent-${AGENT_ARCH}"
echo "🚀 部署 My VPS Probe 被控端（${OS_NAME} / ${AGENT_ARCH}）"

ensure_downloader
mkdir -p /etc/probe
chmod 0755 /etc/probe
TMP_AGENT=$(mktemp /etc/probe/.probe-agent.XXXXXX)
trap 'rm -f "$TMP_AGENT"' EXIT HUP INT TERM

echo "📥 下载被控程序: $DL_URL"
download_file "$DL_URL" "$TMP_AGENT" || fail "被控程序下载失败，请检查主控地址和 HTTPS 证书"
[ -s "$TMP_AGENT" ] || fail "下载到的被控程序为空"
chmod 0755 "$TMP_AGENT"
"$TMP_AGENT" -h >/dev/null 2>&1 || fail "下载文件不是当前架构可执行的 Agent"
mv -f "$TMP_AGENT" /etc/probe/probe-agent
trap - EXIT HUP INT TERM

{
  printf 'PROBE_SERVER='
  shell_quote "$SERVER"
  printf '\nPROBE_TOKEN='
  shell_quote "$TOKEN"
  printf '\n'
} > /etc/probe/agent.env
chmod 0600 /etc/probe/agent.env

install_systemd() {
  cat > /etc/systemd/system/probe-agent.service <<'EOF'
[Unit]
Description=My VPS Probe Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/probe/agent.env
ExecStart=/etc/probe/probe-agent
Restart=always
RestartSec=3
User=root
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable probe-agent >/dev/null 2>&1
  systemctl restart probe-agent
  systemctl is-active --quiet probe-agent || fail "systemd 服务启动失败，请执行 systemctl status probe-agent"
  echo "✅ systemd 服务已启用"
}

install_openrc() {
  if ! command -v rc-service >/dev/null 2>&1; then
    command -v apk >/dev/null 2>&1 || fail "系统没有 systemd 或 OpenRC"
    apk add --no-cache openrc >/dev/null
  fi
  mkdir -p /etc/init.d /etc/conf.d /var/log
  cp /etc/probe/agent.env /etc/conf.d/probe-agent
  chmod 0600 /etc/conf.d/probe-agent
  cat > /etc/init.d/probe-agent <<'EOF'
#!/sbin/openrc-run
name="My VPS Probe Agent"
description="My VPS Probe monitored node agent"
command="/etc/probe/probe-agent"
supervisor="supervise-daemon"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/probe-agent.log"
error_log="/var/log/probe-agent.log"
respawn_delay=3
respawn_max=0
export PROBE_SERVER PROBE_TOKEN

depend() {
  need net
  after firewall
}

start_pre() {
  checkpath --file --mode 0600 "$output_log"
}
EOF
  chmod 0755 /etc/init.d/probe-agent
  rc-update add probe-agent default >/dev/null 2>&1
  rc-service probe-agent restart >/dev/null 2>&1 || rc-service probe-agent start >/dev/null 2>&1
  rc-service probe-agent status >/dev/null 2>&1 || fail "OpenRC 服务启动失败，请执行 rc-service probe-agent status"
  echo "✅ OpenRC 服务已启用"
}

echo "⚙️ 配置常驻服务"
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  install_systemd
elif command -v rc-service >/dev/null 2>&1 || command -v apk >/dev/null 2>&1; then
  install_openrc
else
  fail "未识别到 systemd 或 OpenRC，无法保证 Agent 开机自启"
fi

echo "=========================================="
echo "🎉 被控端部署完成，Agent 将自动连接主控"
echo "=========================================="
