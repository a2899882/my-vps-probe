#!/bin/bash
set -e

GREEN="\033[32m"
RED="\033[31m"
YELLOW="\033[33m"
CYAN="\033[36m"
RESET="\033[0m"

detect_repo() {
  for d in /opt/my-vps-probe /root/my-vps-probe "$HOME/my-vps-probe"; do
    [ -d "$d/.git" ] && { echo "$d"; return 0; }
  done
  return 1
}

get_go_bin() {
  if [ -x /usr/local/go/bin/go ]; then
    echo /usr/local/go/bin/go
  else
    command -v go
  fi
}

get_service_bin() {
  systemctl cat probe-server 2>/dev/null | awk -F= '/^ExecStart=/{print $2; exit}' | awk '{print $1}'
}

update_master() {
  REPO_DIR="$(detect_repo || true)"
  GO_BIN="$(get_go_bin || true)"
  BIN_PATH="$(get_service_bin || true)"

  if [ -z "$REPO_DIR" ]; then
    echo -e "${RED}❌ 找不到主控仓库目录${RESET}"
    read -n 1 -s -r -p "按任意键返回..."
    return
  fi

  if [ -z "$GO_BIN" ]; then
    echo -e "${RED}❌ 找不到 go 命令${RESET}"
    read -n 1 -s -r -p "按任意键返回..."
    return
  fi

  [ -n "$BIN_PATH" ] || BIN_PATH="$REPO_DIR/probe-server"

  cd "$REPO_DIR"
  git fetch origin
  git checkout main
  git pull --ff-only origin main
  "$GO_BIN" build -o "$BIN_PATH" ./server

  systemctl daemon-reload
  systemctl restart probe-server

  echo -e "${GREEN}✅ 主控更新完成${RESET}"
  systemctl --no-pager --full status probe-server || true
  read -n 1 -s -r -p "按任意键返回..."
}

restart_master() {
  systemctl restart probe-server
  echo -e "${GREEN}✅ 已重启主控服务${RESET}"
  sleep 2
}

status_master() {
  systemctl --no-pager --full status probe-server || true
  echo
  read -n 1 -s -r -p "按任意键返回..."
}

bind_domain() {
  read -p "输入要绑定的域名: " domain
  apt-get update
  apt-get install -y caddy
  cat > /etc/caddy/Caddyfile <<EOF
$domain {
  reverse_proxy localhost:8080
}
EOF
  systemctl restart caddy
  echo -e "${GREEN}✅ 域名反代已配置${RESET}"
  sleep 2
}

block_8080() {
  ufw allow ssh
  ufw allow http
  ufw allow https
  ufw deny 8080
  ufw --force enable
  echo -e "${GREEN}✅ 已封锁 8080 直连${RESET}"
  sleep 2
}

allow_8080() {
  ufw allow 8080
  ufw reload
  echo -e "${GREEN}✅ 已放行 8080 直连${RESET}"
  sleep 2
}

uninstall_master() {
  read -p "确认卸载主控端? [y/N]: " ans
  [[ "$ans" =~ ^[Yy]$ ]] || return

  REPO_DIR="$(detect_repo || true)"

  systemctl stop probe-server 2>/dev/null || true
  systemctl disable probe-server 2>/dev/null || true
  rm -f /etc/systemd/system/probe-server.service
  systemctl daemon-reload
  systemctl reset-failed

  [ -n "$REPO_DIR" ] && rm -rf "$REPO_DIR"

  echo -e "${GREEN}✅ 主控端已卸载${RESET}"
  exit 0
}

uninstall_agent() {
  read -p "确认卸载本机被控端? [y/N]: " ans
  [[ "$ans" =~ ^[Yy]$ ]] || return

  systemctl stop probe-agent 2>/dev/null || true
  systemctl disable probe-agent 2>/dev/null || true
  rm -f /etc/systemd/system/probe-agent.service
  rm -f /etc/probe/probe-agent
  rm -rf /etc/probe
  systemctl daemon-reload
  systemctl reset-failed

  echo -e "${GREEN}✅ 本机被控端已卸载${RESET}"
  read -n 1 -s -r -p "按任意键返回..."
}

show_menu() {
  clear
  echo -e "${CYAN}======================================================${RESET}"
  echo -e "${GREEN}          ✨ 极简私有探针 - 主控端管理面板 ✨         ${RESET}"
  echo -e "${CYAN}======================================================${RESET}"
  echo -e " 1. 更新探针主控端"
  echo -e " 2. 重启探针主控端服务"
  echo -e " 3. 查看探针主控端状态"
  echo -e " 4. 添加域名访问 (自动配置 HTTPS 反代)"
  echo -e " 5. 阻止 IP+8080 端口直接访问"
  echo -e " 6. 允许 IP+8080 端口直接访问"
  echo -e " 7. 卸载探针主控端"
  echo -e " 8. 卸载本机被控端"
  echo -e " 0. 退出"
  echo -e "${CYAN}======================================================${RESET}"

  read -p "请输入你的选择: " choice
  case "$choice" in
    1) update_master ;;
    2) restart_master ;;
    3) status_master ;;
    4) bind_domain ;;
    5) block_8080 ;;
    6) allow_8080 ;;
    7) uninstall_master ;;
    8) uninstall_agent ;;
    0) exit 0 ;;
    *) ;;
  esac
  show_menu
}

show_menu
