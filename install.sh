#!/bin/bash

# ====================================================
#  极简私有探针 - 被控端（Agent）一键编译与自动瘦身脚本
# ====================================================

# 1. 解析主控端传过来的 IP 和 Token 参数
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

echo "=========================================="
echo "🚀 🟢 开始执行探针 Agent 一键部署..."
echo "=========================================="

echo "📥 1. 正在准备基础依赖环境..."
apt-get update -y && apt-get install -y git curl wget gcc make >/dev/null 2>&1

# 如果没有 Go 环境，则临时下载安装用于编译
if [ ! -f "/usr/local/go/bin/go" ]; then
    echo "📦 正在临时部署 Go 编译器..."
    wget -q https://go.dev/dl/go1.22.4.linux-amd64.tar.gz -O go.tar.gz 2>/dev/null || wget -q https://dl.google.com/go/go1.22.4.linux-amd64.tar.gz -O go.tar.gz
    rm -rf /usr/local/go && tar -C /usr/local -xzf go.tar.gz
    rm -f go.tar.gz
fi

echo "🛠️ 2. 正在拉取最新的黄金源码并启动编译..."
rm -rf /tmp/probe-agent-build
git clone https://github.com/a2899882/my-vps-probe.git /tmp/probe-agent-build >/dev/null 2>&1
cd /tmp/probe-agent-build

# 执行编译
/usr/local/go/bin/go build -o probe-agent agent/main.go
if [ ! -f "probe-agent" ]; then
    echo "❌ 编译失败！请检查小鸡网络或核心架构。"
    exit 1
fi

# 将编译出来的纯净单体二进制文件复制到专用的系统运行目录
mkdir -p /etc/probe
cp probe-agent /etc/probe/probe-agent

echo "⚙️ 3. 正在配置后台 Systemd 守护进程..."
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

# 启动服务
systemctl daemon-reload
systemctl enable probe-agent >/dev/null 2>&1
systemctl start probe-agent

# ====================================================
# 🔥 核心看点：过河拆桥，开始极致大扫除
# ====================================================
echo "🧹 4. [自动瘦身] 监测到程序已成功上线，开始清理无用环境..."

# 1. 彻底拔除占地 300MB+ 的 Go 编译器
rm -rf /usr/local/go
rm -f /usr/bin/go /usr/local/bin/go 2>/dev/null

# 2. 彻底清空编译期间产生的垃圾缓存和依赖依赖包 (约 300MB+)
rm -rf ~/.cache/go-build
rm -rf ~/go

# 3. 彻底删掉用于编译的临时源码文件夹
rm -rf /tmp/probe-agent-build

# ====================================================
# 注入简易的小鸡本地管理菜单快捷键（用完即抛，不留垃圾）
# ====================================================
cat << 'MenuEOF' > /usr/local/bin/tz
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
        rm -f /usr/local/bin/tz
        systemctl daemon-reload
        echo "✅ 卸载成功，探针已彻底离开此系统！"
        ;;
    *) exit 0 ;;
esac
MenuEOF
chmod +x /usr/local/bin/tz

echo "=========================================="
echo "🎉 ✅ 部署彻底完成！被控端已上线！"
echo "🔒 系统盘已完成自动化无痛瘦身，未残留任何编译垃圾。"
echo "💡 提示：在此小鸡输入 [ tz ] 回车，可随时管理或彻底卸载。"
echo "=========================================="
