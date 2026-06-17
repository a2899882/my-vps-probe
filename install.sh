#!/bin/bash
while getopts s:t: flag; do
    case "${flag}" in
        s) server_addr=${OPTARG};;
        t) node_token=${OPTARG};;
    esac
done
GREEN="\033[32m"
YELLOW="\033[33m"
RESET="\033[0m"
echo -e "${GREEN}🚀 开始执行单文件探针 Agent 一键部署...${RESET}"
if [ -z "$server_addr" ] || [ -z "$node_token" ]; then
    read -p "请输入主控端地址: " server_addr
    read -p "请输入该节点的 Token: " node_token
fi
echo -e "${YELLOW}1. 正在准备环境...${RESET}"
apt update -y > /dev/null 2>&1
apt install -y git wget curl > /dev/null 2>&1
if ! command -v go &> /dev/null; then
    wget -q https://dl.google.com/go/go1.22.4.linux-amd64.tar.gz
    rm -rf /usr/local/go && tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
    ln -sf /usr/local/go/bin/go /usr/bin/go
    rm go1.22.4.linux-amd64.tar.gz
fi

echo -e "${YELLOW}2. 正在拉取超轻量单文件并编译...${RESET}"
rm -rf /opt/my-vps-probe && mkdir -p /opt/my-vps-probe/agent && cd /opt/my-vps-probe
go mod init my-vps-probe > /dev/null 2>&1
go get github.com/gorilla/websocket github.com/shirou/gopsutil/v3/cpu github.com/shirou/gopsutil/v3/disk github.com/shirou/gopsutil/v3/host github.com/shirou/gopsutil/v3/load github.com/shirou/gopsutil/v3/mem github.com/shirou/gopsutil/v3/net > /dev/null 2>&1

# 彻底摒弃复杂依赖，只拉取 agent/main.go
wget -qO agent/main.go http://${server_addr}/download/agent.go

go build -o probe-agent agent/main.go
mv probe-agent /usr/local/bin/

echo -e "${YELLOW}3. 配置后台守护进程...${RESET}"
cat << SYSTEMD > /etc/systemd/system/probe-agent.service
[Unit]
Description=My VPS Probe Agent
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/probe-agent -server ${server_addr} -token ${node_token}
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
SYSTEMD
systemctl daemon-reload
systemctl enable probe-agent > /dev/null 2>&1
systemctl restart probe-agent
echo -e "${GREEN}✅ 部署完成！被控端已上线！${RESET}"
