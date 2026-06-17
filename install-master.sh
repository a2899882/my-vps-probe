#!/bin/bash
GREEN="\033[32m"
YELLOW="\033[33m"
CYAN="\033[36m"
RESET="\033[0m"

echo -e "${CYAN}=================================================${RESET}"
echo -e "${GREEN}   ✨ 极简私有探针 - 主控端一键全自动部署 ✨   ${RESET}"
echo -e "${CYAN}=================================================${RESET}"

echo -e "${YELLOW}1. 正在安装基础环境 (Go / Git / Curl)...${RESET}"
apt update -y > /dev/null 2>&1
apt install -y git wget curl ufw > /dev/null 2>&1

if ! command -v go &> /dev/null; then
    wget -q https://dl.google.com/go/go1.22.4.linux-amd64.tar.gz
    rm -rf /usr/local/go && tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
    ln -sf /usr/local/go/bin/go /usr/bin/go
    rm go1.22.4.linux-amd64.tar.gz
fi

echo -e "${YELLOW}2. 正在拉取探针主控端源码...${RESET}"
# 如果目录已存在则先备份
if [ -d "/root/my-vps-probe" ]; then
    mv /root/my-vps-probe /root/my-vps-probe_bak_$(date +%s)
fi

# 提示输入 GitHub 仓库地址进行拉取
repo_url="https://github.com/a2899882/my-vps-probe.git"
git clone "$repo_url" /root/my-vps-probe
cd /root/my-vps-probe
go mod tidy > /dev/null 2>&1

echo -e "${YELLOW}3. 正在生成全局控制台指令 (tz)...${RESET}"
cat << 'TZSCRIPT' > /usr/local/bin/tz
#!/bin/bash
GREEN="\033[32m"
RED="\033[31m"
YELLOW="\033[33m"
CYAN="\033[36m"
RESET="\033[0m"
show_menu() {
    clear
    echo -e "${CYAN}======================================================${RESET}"
    echo -e "${GREEN}          ✨ 极简私有探针 - 主控端管理面板 ✨         ${RESET}"
    echo -e "${CYAN}======================================================${RESET}"
    echo -e " 1. 重启探针主控端服务"
    echo -e " 2. 卸载探针主控端"
    echo -e " 5. 添加域名访问 (自动配置 HTTPS 反代)"
    echo -e " 7. 阻止 IP+8080 端口直接访问 (安全防爆破)"
    echo -e " 8. 允许 IP+8080 端口直接访问"
    echo -e " 0. 退出"
    echo -e "${CYAN}======================================================${RESET}"
    read -p "请输入你的选择: " choice
    case $choice in
        1) systemctl restart probe-server; echo "✅ 已重启！"; sleep 2; show_menu ;;
        2) systemctl stop probe-server && systemctl disable probe-server; echo "✅ 已卸载！"; sleep 2; exit 0 ;;
        5) read -p "输入要绑定的域名: " domain; apt install caddy -y; echo -e "$domain {\n reverse_proxy localhost:8080\n}" > /etc/caddy/Caddyfile; systemctl restart caddy; echo "✅ 绑定成功！"; sleep 2; show_menu ;;
        7) ufw allow ssh; ufw allow http; ufw allow https; ufw deny 8080; ufw --force enable; echo "✅ 8080 已封锁！"; sleep 2; show_menu ;;
        8) ufw allow 8080; ufw reload; echo "✅ 8080 已放行！"; sleep 2; show_menu ;;
        0) exit 0 ;;
        *) show_menu ;;
    esac
}
show_menu
TZSCRIPT
chmod +x /usr/local/bin/tz

echo -e "${YELLOW}4. 正在配置主控端 Systemd 守护进程...${RESET}"
cat << SYS > /etc/systemd/system/probe-server.service
[Unit]
Description=My VPS Probe Server
After=network.target

[Service]
Type=simple
WorkingDirectory=/root/my-vps-probe
ExecStart=/usr/local/go/bin/go run server/main.go
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
SYS

systemctl daemon-reload
systemctl enable probe-server > /dev/null 2>&1
systemctl restart probe-server

echo -e "${CYAN}=================================================${RESET}"
echo -e "${GREEN} 🎉 主控端部署彻底完成！ 🎉 ${RESET}"
echo -e "${CYAN}=================================================${RESET}"
echo -e "1. 您的默认后台账号: ${YELLOW}admin${RESET} 密码: ${YELLOW}123456${RESET} (请登录后立即修改)"
echo -e "2. 请在终端输入 ${GREEN}tz${RESET} 回车，即可调出全局控制面板绑定您的域名！"
echo -e "${CYAN}=================================================${RESET}"
