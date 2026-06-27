#!/bin/bash
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

echo "🚀 开始极速部署被控端 (本地主控全闭环版)..."

CLEAN_SERVER=$(echo "$SERVER" | sed 's/http:\/\///g' | sed 's/https:\/\///g')
BASE_URL="https://${CLEAN_SERVER}"

ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    DL_URL="${BASE_URL}/probe-agent-amd64?v=4"
elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    DL_URL="${BASE_URL}/probe-agent-arm64?v=4"
else
    echo "❌ 暂不支持的架构: $ARCH"
    exit 1
fi

echo "📥 正在拉取核心成品 ($DL_URL) ..."
mkdir -p /etc/probe

# 【核心修复】：获取下载请求的真实 HTTP 状态码，绝对不再去扫文件内容！
HTTP_CODE=$(curl -sL -w "%{http_code}" "$DL_URL" -o /etc/probe/probe-agent)

if [ "$HTTP_CODE" != "200" ]; then
    echo "❌ 核心程序拉取失败！HTTP 状态码: $HTTP_CODE"
    rm -f /etc/probe/probe-agent
    exit 1
fi

chmod +x /etc/probe/probe-agent

echo "⚙️ 正在配置并启动探针服务..."
cat << SystemdEOF > /etc/systemd/system/probe-agent.service
[Unit]
Description=My VPS Probe Agent
After=network.target

[Service]
Type=simple
ExecStart=/etc/probe/probe-agent -server ${SERVER} -token ${TOKEN}
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
SystemdEOF

systemctl daemon-reload
systemctl enable probe-agent >/dev/null 2>&1
systemctl restart probe-agent

echo "=========================================="
echo "🎉 部署彻底完成！探针已成功连线！"
echo "=========================================="
