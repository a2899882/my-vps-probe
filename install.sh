#!/usr/bin/env bash
set -euo pipefail
SERVER=""
TOKEN=""
while getopts "s:t:" opt; do
  case $opt in
    s) SERVER=$OPTARG ;;
    t) TOKEN=$OPTARG ;;
  esac
done

if [ -z "$SERVER" ] || [ -z "$TOKEN" ]; then
  echo "❌ 错误: 缺少参数！"
  exit 1
fi

echo "🚀 开始部署 My VPS Probe 被控端..."

if [[ "$SERVER" =~ ^https?:// ]]; then
    BASE_URL="${SERVER%/}"
else
    BASE_URL="https://${SERVER%/}"
fi

ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    DL_URL="${BASE_URL}/probe-agent-amd64"
elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    DL_URL="${BASE_URL}/probe-agent-arm64"
else
    echo "❌ 暂不支持的架构: $ARCH"
    exit 1
fi

echo "📥 正在拉取核心成品 ($DL_URL) ..."
install -d -m 0755 /etc/probe
TMP_AGENT="$(mktemp /etc/probe/probe-agent.XXXXXX)"
trap 'rm -f "$TMP_AGENT"' EXIT

# 【核心修复】：获取下载请求的真实 HTTP 状态码，绝对不再去扫文件内容！
HTTP_CODE=$(curl -sSL --retry 3 --connect-timeout 10 --max-time 90 -w "%{http_code}" "$DL_URL" -o "$TMP_AGENT")

if [ "$HTTP_CODE" != "200" ]; then
    echo "❌ 核心程序拉取失败！HTTP 状态码: $HTTP_CODE"
    exit 1
fi

install -m 0755 "$TMP_AGENT" /etc/probe/probe-agent
rm -f "$TMP_AGENT"
trap - EXIT

install -m 0600 /dev/null /etc/probe/agent.env
printf 'PROBE_SERVER=%s\nPROBE_TOKEN=%s\n' "$SERVER" "$TOKEN" > /etc/probe/agent.env

echo "⚙️ 正在配置并启动探针服务..."
cat << SystemdEOF > /etc/systemd/system/probe-agent.service
[Unit]
Description=My VPS Probe Agent
After=network.target

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
SystemdEOF

systemctl daemon-reload
systemctl enable probe-agent >/dev/null 2>&1
systemctl restart probe-agent

echo "=========================================="
echo "🎉 部署彻底完成！探针已成功连线！"
echo "=========================================="
