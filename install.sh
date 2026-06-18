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
  echo "❌ 错误: 缺少参数！正确用法: install.sh -s 主控IP:端口 -t 通信Token"
  exit 1
fi

echo "🚀 开始极速部署被控端 (本地主控全闭环版)..."

# 【核心进化】：提取用户输入的主控连接地址，直接作为下载源！
# 如果 SERVER 传入的是带 http 的域名，直接用；如果是纯 IP:端口，则拼上 http
if [[ "$SERVER" =~ "http" ]]; then
    BASE_URL="${SERVER}"
else
    BASE_URL="http://${SERVER}"
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

echo "📥 正在直接从你的主控机极速拉取成品 ($DL_URL) ..."
mkdir -p /etc/probe
curl -sL $DL_URL -o /etc/probe/probe-agent || wget -qO /etc/probe/probe-agent $DL_URL

if [ ! -s "/etc/probe/probe-agent" ] || grep -q "404" /etc/probe/probe-agent; then
    echo "❌ 核心程序拉取失败！请检查主控端对应文件是否存在。"
    rm -f /etc/probe/probe-agent
    exit 1
fi

chmod +x /etc/probe/probe-agent

echo "⚙️ 正在配置后台服务..."
cat << SystemdEOF > /etc/systemd/system/probe-agent.service
[Unit]
Description=My VPS Probe Agent
After=network.target

[Service]
Type=simple
ExecStart=/etc/probe/probe-agent -server $SERVER -token $TOKEN
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
SystemdEOF

systemctl daemon-reload
systemctl enable probe-agent >/dev/null 2>&1
systemctl restart probe-agent

cat << 'MenuEOF' > /usr/local/bin/tza
#!/bin/bash
echo "=========================================="
echo "       极简私有探针 - 被控端本地管理      "
echo "=========================================="
echo " 1. 查看小鸡探针运行状态"
echo " 2. 查看实时通信日志"
echo " 3. 彻底卸载此小鸡探针"
echo " 0. 退出菜单"
echo "=========================================="
read -p "请输入你的选择 (0-3): " choice
case $choice in
    1) systemctl status probe-agent ;;
    2) journalctl -u probe-agent -n 30 --no-pager ;;
    3) 
        systemctl stop probe-agent
        systemctl disable probe-agent
        rm -f /etc/systemd/system/probe-agent.service
        rm -rf /etc/probe
        rm -f /usr/local/bin/tza
        systemctl daemon-reload
        echo "✅ 卸载成功，探针已彻底离开此系统！"
        ;;
    *) exit 0 ;;
esac
MenuEOF
chmod +x /usr/local/bin/tza

echo "=========================================="
echo "🎉 部署彻底完成！探针已秒级上线！"
echo "💡 提示：此版本为完全闭环版，不依赖任何第三方平台。"
echo "=========================================="
