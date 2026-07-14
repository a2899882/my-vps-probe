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

create_master_backup() {
  local repo_dir="$1"
  local backup_dir="$repo_dir/backups"
  local stamp work_dir backup_file rc
  local files=(config.json data.db)

  stamp="$(date +%Y%m%d-%H%M%S)"
  work_dir="$(mktemp -d)"
  backup_file="$backup_dir/my-vps-probe-backup-${stamp}.tar.gz"
  mkdir -p "$backup_dir"

  if [ ! -f "$repo_dir/config.json" ] || [ ! -f "$repo_dir/data.db" ]; then
    echo -e "${RED}❌ 找不到 config.json 或 data.db，无法备份${RESET}" >&2
    rm -rf "$work_dir"
    return 1
  fi

  install -m 600 "$repo_dir/config.json" "$work_dir/config.json"
  install -m 600 "$repo_dir/data.db" "$work_dir/data.db"

  if [ -f "$repo_dir/usage_state.json" ]; then
    install -m 600 "$repo_dir/usage_state.json" "$work_dir/usage_state.json"
    files+=(usage_state.json)
  fi

  cat > "$work_dir/manifest.txt" <<EOF
format=my-vps-probe-backup-v1
created_at=$(date -Is)
hostname=$(hostname)
EOF
  files+=(manifest.txt)

  (
    cd "$work_dir"
    sha256sum "${files[@]}" > SHA256SUMS
    tar -czf "$backup_file" "${files[@]}" SHA256SUMS
  )
  rc=$?
  rm -rf "$work_dir"

  [ "$rc" -eq 0 ] || return "$rc"
  printf '%s\n' "$backup_file"
}

backup_master() {
  local repo_dir backup_file rc

  repo_dir="$(detect_repo || true)"
  [ -n "$repo_dir" ] || { echo -e "${RED}❌ 找不到主控仓库目录${RESET}"; return; }

  echo -e "${YELLOW}正在短暂停止主控服务，以创建一致性备份...${RESET}"
  if ! systemctl stop probe-server; then
    echo -e "${RED}❌ 无法停止主控服务，已取消备份${RESET}"
    return
  fi

  backup_file="$(create_master_backup "$repo_dir")"
  rc=$?
  systemctl start probe-server || true

  if [ "$rc" -ne 0 ] || [ -z "$backup_file" ]; then
    echo -e "${RED}❌ 备份失败；主控服务已尝试重新启动${RESET}"
    return
  fi

  echo -e "${GREEN}✅ 备份完成${RESET}"
  echo "文件: $backup_file"
  ls -lh "$backup_file"
  read -n 1 -s -r -p "按任意键返回..."
}

restore_master() {
  local repo_dir backup_file tmp_dir confirm protection_backup rc
  local archive_list expected_minimal expected_full

  repo_dir="$(detect_repo || true)"
  [ -n "$repo_dir" ] || { echo -e "${RED}❌ 找不到主控仓库目录${RESET}"; return; }

  read -r -p "输入已上传备份包的完整路径: " backup_file
  [ -f "$backup_file" ] || { echo -e "${RED}❌ 备份文件不存在${RESET}"; return; }

  archive_list="$(tar -tzf "$backup_file" 2>/dev/null)" || {
    echo -e "${RED}❌ 无法读取备份压缩包${RESET}"
    return
  }

  expected_minimal="$(printf '%s\n' config.json data.db manifest.txt SHA256SUMS | sort)"
  expected_full="$(printf '%s\n' config.json data.db usage_state.json manifest.txt SHA256SUMS | sort)"

  if [ "$(printf '%s\n' "$archive_list" | sort)" != "$expected_minimal" ] &&
     [ "$(printf '%s\n' "$archive_list" | sort)" != "$expected_full" ]; then
    echo -e "${RED}❌ 备份包文件列表异常，已拒绝恢复${RESET}"
    return
  fi

  tmp_dir="$(mktemp -d)"
  if ! tar -xzf "$backup_file" -C "$tmp_dir"; then
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 解压失败，未恢复任何文件${RESET}"
    return
  fi

  if ! grep -qx 'format=my-vps-probe-backup-v1' "$tmp_dir/manifest.txt" ||
     ! (cd "$tmp_dir" && sha256sum -c SHA256SUMS); then
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 备份格式或校验失败，未恢复任何文件${RESET}"
    return
  fi

  read -r -p "将覆盖当前主控配置和历史数据，输入 RESTORE 确认: " confirm
  [ "$confirm" = "RESTORE" ] || { rm -rf "$tmp_dir"; echo "已取消恢复"; return; }

  echo -e "${YELLOW}正在创建恢复前保护备份...${RESET}"
  if ! systemctl stop probe-server; then
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 无法停止主控服务，已取消恢复${RESET}"
    return
  fi

  protection_backup="$(create_master_backup "$repo_dir")"
  rc=$?
  if [ "$rc" -ne 0 ] || [ -z "$protection_backup" ]; then
    systemctl start probe-server || true
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 恢复前保护备份失败，已取消恢复${RESET}"
    return
  fi

  install -m 600 "$tmp_dir/config.json" "$repo_dir/config.json"
  install -m 600 "$tmp_dir/data.db" "$repo_dir/data.db"

  if [ -f "$tmp_dir/usage_state.json" ]; then
    install -m 600 "$tmp_dir/usage_state.json" "$repo_dir/usage_state.json"
  else
    rm -f "$repo_dir/usage_state.json"
  fi

  rm -rf "$tmp_dir"
  systemctl start probe-server
  sleep 2

  if systemctl is-active --quiet probe-server; then
    echo -e "${GREEN}✅ 恢复完成，主控服务已启动${RESET}"
    echo "恢复前保护备份: $protection_backup"
  else
    echo -e "${RED}❌ 文件已恢复，但服务未正常启动${RESET}"
    echo "可用保护备份回退: $protection_backup"
    echo "请执行: systemctl status probe-server"
  fi
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
  echo -e " 9. 备份主控配置与历史数据"
  echo -e "10. 从备份恢复主控配置与历史数据"
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
    9) backup_master ;;
    10) restore_master ;;
    0) exit 0 ;;
    *) ;;
  esac
  show_menu
}

show_menu
