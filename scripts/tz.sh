#!/usr/bin/env bash
set -e

GREEN="\033[32m"
RED="\033[31m"
YELLOW="\033[33m"
CYAN="\033[36m"
BLUE="\033[34m"
RESET="\033[0m"

CADDY_FILE="/etc/caddy/Caddyfile"

# 确保任何退出路径（正常退出、Ctrl+C、EOF）均恢复终端正常回显与光标状态，绝不清空 SSH 历史缓存
restore_tty() {
  stty sane </dev/tty >/dev/null 2>&1 || stty sane 2>/dev/null || true
  printf '\033[0m\033[?25h'
}
trap 'restore_tty; echo ""' EXIT
trap 'restore_tty; echo ""; exit 130' INT TERM

pause_return() {
  restore_tty
  if [[ -r /dev/tty ]]; then
    echo ""
    read -r -p "按回车键返回主菜单..." _ </dev/tty || true
  fi
}

detect_repo() {
  for d in /opt/my-vps-probe /root/my-vps-probe "$HOME/my-vps-probe"; do
    if [ -d "$d" ] && { [ -f "$d/server/main.go" ] || [ -f "$d/probe-server" ] || [ -d "$d/.git" ]; }; then
      echo "$d"
      return 0
    fi
  done
  return 1
}

get_go_bin() {
  if [ -x /usr/local/go/bin/go ]; then
    echo /usr/local/go/bin/go
  else
    command -v go 2>/dev/null || true
  fi
}

get_service_bin() {
  local bin=""
  bin="$(systemctl cat probe-server 2>/dev/null | awk -F= '/^ExecStart=/{print $2; exit}' | awk '{print $1}' || true)"
  if [ -n "$bin" ]; then
    echo "$bin"
  else
    local repo
    repo="$(detect_repo || echo "/opt/my-vps-probe")"
    echo "$repo/probe-server"
  fi
}

get_proc_rss_mb() {
  local pattern="$1"
  local pid rss_kb
  pid="$(pgrep -f "$pattern" 2>/dev/null | head -n 1 || true)"
  if [ -n "$pid" ] && [ -r "/proc/$pid/status" ]; then
    rss_kb="$(awk '/^VmRSS:/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)"
    if [ "${rss_kb:-0}" -gt 0 ]; then
      awk "BEGIN {printf \"%.1f MB\", $rss_kb/1024}"
      return 0
    fi
  fi
  echo "-"
}

get_probe_domain() {
  local repo
  repo="$(detect_repo || true)"
  if [ -n "$repo" ] && [ -f "$repo/config.json" ]; then
    local d
    d="$(grep -Eo '"panel_domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$repo/config.json" 2>/dev/null | head -n 1 | sed -E 's/.*"([^"]*)"$/\1/' || true)"
    if [ -n "$d" ]; then
      echo "$d"
      return 0
    fi
  fi
  if [ -f "$CADDY_FILE" ]; then
    if grep -q "# BEGIN MY-VPS-PROBE" "$CADDY_FILE" 2>/dev/null; then
      sed -n '/# BEGIN MY-VPS-PROBE/,/# END MY-VPS-PROBE/p' "$CADDY_FILE" | awk '/\{/{print $1; exit}'
      return 0
    fi
    awk '/\{/{dom=$1} /8080/{if(dom!="") {print dom; exit}}' "$CADDY_FILE" 2>/dev/null || true
  fi
}

get_probe_ip_lock() {
  local repo
  repo="$(detect_repo || true)"
  if [ -n "$repo" ] && [ -f "$repo/config.json" ]; then
    if grep -Eq '"block_ip_access"[[:space:]]*:[[:space:]]*true' "$repo/config.json" 2>/dev/null; then
      echo "已阻止 IP+8080 直连 (仅限域名访问)"
      return 0
    fi
  fi
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "8080.*DENY"; then
    echo "已通过防火墙阻止 8080 直连"
    return 0
  fi
  echo "允许 IP+8080 直接访问"
}

migrate_caddyfile_probe_block() {
  [ -f "$CADDY_FILE" ] || return 0
  if grep -q "# BEGIN MY-VPS-PROBE" "$CADDY_FILE" 2>/dev/null; then
    return 0
  fi
  if grep -Eq "localhost:8080|127\.0\.0\.1:8080" "$CADDY_FILE" 2>/dev/null; then
    python3 - "$CADDY_FILE" <<'PY' 2>/dev/null || true
import re, sys
path = sys.argv[1]
raw = open(path, 'r', encoding='utf-8', errors='ignore').read()
if '# BEGIN MY-VPS-PROBE' in raw:
    sys.exit(0)
pattern = re.compile(r'(?m)^([a-zA-Z0-9._-]+)\s*\{\s*(?:[^\n{}]*\n)*?\s*reverse_proxy\s+(?:localhost|127\.0\.0\.1):8080\s*\n\}')
m = pattern.search(raw)
if m:
    dom = m.group(1)
    block = f"# BEGIN MY-VPS-PROBE\n{dom} {{\n    encode zstd gzip\n    reverse_proxy 127.0.0.1:8080\n}}\n# END MY-VPS-PROBE"
    new_raw = raw[:m.start()] + block + raw[m.end():]
    open(path, 'w', encoding='utf-8').write(new_raw)
PY
  fi
}

sync_systemd_low_mem() {
  local repo_dir="$1"
  local bin_path="$2"
  [ -n "$repo_dir" ] || return 0
  [ -n "$bin_path" ] || bin_path="$repo_dir/probe-server"
  if [ -d /run/systemd/system ]; then
    cat > /etc/systemd/system/probe-server.service <<EOF
[Unit]
Description=My VPS Probe Server
After=network.target

[Service]
Type=simple
WorkingDirectory=${repo_dir}
Environment="GOGC=75"
Environment="GOMEMLIMIT=64MiB"
ExecStart=${bin_path}
Restart=always
RestartSec=3
User=root
NoNewPrivileges=true
PrivateTmp=true
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload 2>/dev/null || true
  fi
}

apply_source_tree_to_repo() {
  local src_dir="$1"
  local repo_dir="$2"
  mkdir -p "$repo_dir/server" "$repo_dir/agent" "$repo_dir/common" "$repo_dir/tcpping" "$repo_dir/scripts" "$repo_dir/tests"
  cp -rf "$src_dir/server/"*.go "$src_dir/server/"*.html "$repo_dir/server/" 2>/dev/null || true
  cp -rf "$src_dir/agent/"* "$repo_dir/agent/" 2>/dev/null || true
  cp -rf "$src_dir/common/"* "$repo_dir/common/" 2>/dev/null || true
  cp -rf "$src_dir/tcpping/"* "$repo_dir/tcpping/" 2>/dev/null || true
  cp -rf "$src_dir/scripts/"* "$repo_dir/scripts/" 2>/dev/null || true
  [ -d "$src_dir/tests" ] && cp -rf "$src_dir/tests/"* "$repo_dir/tests/" 2>/dev/null || true
  for f in go.mod go.sum install.sh install-master.sh uninstall-agent.sh README.md; do
    [ -f "$src_dir/$f" ] && cp -f "$src_dir/$f" "$repo_dir/$f"
  done
  chmod +x "$repo_dir/install.sh" "$repo_dir/install-master.sh" "$repo_dir/scripts/tz.sh" 2>/dev/null || true
}

update_master() {
  local custom_src="${1:-}"
  local REPO_DIR GO_BIN BIN_PATH build_dir
  REPO_DIR="$(detect_repo || true)"
  GO_BIN="$(get_go_bin || true)"
  BIN_PATH="$(get_service_bin || true)"

  if [ -z "$REPO_DIR" ]; then
    echo -e "${RED}❌ 找不到主控仓库目录 (/opt/my-vps-probe)${RESET}"
    pause_return
    return
  fi

  if [ -z "$GO_BIN" ]; then
    echo -e "${RED}❌ 找不到 go 命令，请先运行 install-master.sh 安装 Go 环境${RESET}"
    pause_return
    return
  fi

  [ -n "$BIN_PATH" ] || BIN_PATH="$REPO_DIR/probe-server"

  # 检测是否存在本地上传的更新包 (/root/my-vps-probe*.zip 或命令行指定路径)
  local local_zip=""
  if [ -n "$custom_src" ] && [ -e "$custom_src" ]; then
    local_zip="$custom_src"
  else
    for cand in /root/my-vps-probe-main.zip /root/my-vps-probe*.zip /tmp/my-vps-probe-main.zip; do
      if [ -f "$cand" ]; then
        local_zip="$cand"
        break
      fi
    done
  fi

  local use_local="n"
  if [ -n "$local_zip" ]; then
    echo -e "${CYAN}📦 检测到本地更新源码包: ${YELLOW}${local_zip}${RESET}"
    if [[ -r /dev/tty && -z "$custom_src" ]]; then
      read -r -p "是否直接使用该本地源码包进行无损热更新？[Y/n (直接回车默认使用本地包，输入 n 从 Git 拉取)]: " use_local </dev/tty || true
    else
      use_local="y"
    fi
  fi

  echo -e "${YELLOW}🛡️ 正在自动创建更新前安全备份...${RESET}"
  create_master_backup "$REPO_DIR" "pre-update" >/dev/null 2>&1 || true

  if [[ "${use_local:-Y}" =~ ^[Yy]?$ ]] && [ -n "$local_zip" ]; then
    local unpack_dir
    unpack_dir="$(mktemp -d)"
    if [ -d "$local_zip" ]; then
      apply_source_tree_to_repo "$local_zip" "$REPO_DIR"
    else
      if ! command -v unzip >/dev/null 2>&1; then
        apt-get update -qq && apt-get install -y -qq unzip >/dev/null 2>&1 || true
      fi
      if ! unzip -oq "$local_zip" -d "$unpack_dir"; then
        rm -rf "$unpack_dir"
        echo -e "${RED}❌ 解压本地源码包失败: $local_zip${RESET}"
        pause_return
        return
      fi
      local inner_src="$unpack_dir"
      [ -d "$unpack_dir/my-vps-probe-main" ] && inner_src="$unpack_dir/my-vps-probe-main"
      if [ ! -f "$inner_src/server/main.go" ]; then
        rm -rf "$unpack_dir"
        echo -e "${RED}❌ 源码包结构不完整（未找到 server/main.go）${RESET}"
        pause_return
        return
      fi
      apply_source_tree_to_repo "$inner_src" "$REPO_DIR"
    fi
    rm -rf "$unpack_dir"
    echo -e "${GREEN}✅ 已同步本地源码至 $REPO_DIR（保留全部配置与历史数据）${RESET}"
  else
    cd "$REPO_DIR"
    if [ -d "$REPO_DIR/.git" ]; then
      echo -e "${CYAN}⬇️ 正在从 Git 仓库拉取最新代码...${RESET}"
      git restore --worktree --staged \
        probe-server probe-agent-amd64 probe-agent-arm64 \
        server/probe-agent-amd64 server/probe-agent-arm64 \
        go1.22.4.linux-amd64.tar.gz 2>/dev/null || true
      if ! git fetch origin || ! git checkout main || ! git pull --ff-only origin main; then
        echo -e "${YELLOW}⚠️ Git 在线拉取未成功，将使用当前 $REPO_DIR 目录源码直接编译更新...${RESET}"
      fi
    else
      echo -e "${CYAN}ℹ️ 当前目录非 Git 仓库，直接使用 $REPO_DIR 现有源码编译...${RESET}"
    fi
  fi

  cd "$REPO_DIR"
  echo -e "${CYAN}🔨 正在以低内存保护模式 (GOMEMLIMIT=220MiB, -p 1) 编译主控与双架构 Agent...${RESET}"
  build_dir="$(mktemp -d)"
  if ! env GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "$build_dir/probe-server" ./server ||
     ! env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "$build_dir/probe-agent-amd64" ./agent ||
     ! env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOGC=50 GOMEMLIMIT=220MiB "$GO_BIN" build -p 1 -trimpath -ldflags="-s -w" -o "$build_dir/probe-agent-arm64" ./agent; then
    rm -rf "$build_dir"
    echo -e "${RED}❌ 编译失败，现有运行中的主控服务未被替换${RESET}"
    pause_return
    return
  fi

  install -m 0755 "$build_dir/probe-server" "$BIN_PATH"
  install -m 0755 "$build_dir/probe-agent-amd64" "$REPO_DIR/server/probe-agent-amd64"
  install -m 0755 "$build_dir/probe-agent-arm64" "$REPO_DIR/server/probe-agent-arm64"
  rm -rf "$build_dir"

  if [ -f "$REPO_DIR/scripts/tz.sh" ]; then
    install -m 0755 "$REPO_DIR/scripts/tz.sh" /usr/local/bin/tz
    ln -sf /usr/local/bin/tz /usr/bin/tz 2>/dev/null || true
  fi

  sync_systemd_low_mem "$REPO_DIR" "$BIN_PATH"
  migrate_caddyfile_probe_block
  systemctl restart probe-server

  local healthy=0
  for _ in {1..20}; do
    if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
      healthy=1
      break
    fi
    sleep 1
  done
  if [ "$healthy" -ne 1 ]; then
    echo -e "${RED}❌ 主控更新后健康检查失败，请查看服务日志${RESET}"
    systemctl --no-pager --full status probe-server || true
    pause_return
    return
  fi

  echo -e "${GREEN}✅ 主控热更新完成！所有节点配置、月流量与历史数据完好无损。${RESET}"
  systemctl --no-pager -n 8 status probe-server || true
  pause_return
}

restart_master() {
  systemctl restart probe-server
  echo -e "${GREEN}✅ 已重启探针主控服务${RESET}"
  sleep 1
}

status_master() {
  echo -e "${CYAN}──────────────── 探针主控运行状态与健康检查 ────────────────${RESET}"
  curl -fsS http://127.0.0.1:8080/healthz 2>/dev/null && echo "" || echo -e "${RED}健康检查接口未响应${RESET}"
  echo ""
  systemctl --no-pager -n 15 status probe-server || true
  pause_return
}

bind_domain() {
  local current_domain
  current_domain="$(get_probe_domain)"
  echo -e "${CYAN}──────────────── 探针域名访问设置 (Caddy 独立块反代 · 多面板零冲突) ────────────────${RESET}"
  echo -e " 当前绑定域名: ${YELLOW}${current_domain:-未绑定}${RESET}"
  read -r -p "输入要绑定的域名 (输入 clear 解绑域名，直接回车取消): " domain </dev/tty || true
  domain="$(echo "$domain" | tr -d '[:space:]' | sed -E 's#^https?://##; s#/.*$##')"
  [ -n "$domain" ] || { echo -e "${YELLOW}⚠️ 域名为空，已取消操作${RESET}"; pause_return; return; }

  migrate_caddyfile_probe_block

  if [ "$domain" = "clear" ]; then
    if [ -f "$CADDY_FILE" ]; then
      sed -i '/# BEGIN MY-VPS-PROBE/,/# END MY-VPS-PROBE/d' "$CADDY_FILE"
      systemctl reload caddy 2>/dev/null || systemctl restart caddy 2>/dev/null || true
    fi
    curl -fsS -X POST "http://127.0.0.1:8080/api/admin/local-op" \
      -H "Content-Type: application/json" \
      -H "X-Probe-Local: 1" \
      -d '{"panel_domain":"","block_ip_access":false}' >/dev/null 2>&1 || true
    echo -e "${GREEN}✅ 已解绑探针域名并恢复 IP+8080 直接访问${RESET}"
    pause_return
    return
  fi

  if ! command -v caddy >/dev/null 2>&1; then
    echo -e "${CYAN}📦 正在安装 Caddy 反代服务...${RESET}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq caddy
  fi

  mkdir -p /etc/caddy
  touch "$CADDY_FILE"

  # 检查是否与本机 Nexus-Flux 或 NexusGate-Plus 域名冲突
  local other_blocks
  other_blocks="$(sed '/# BEGIN MY-VPS-PROBE/,/# END MY-VPS-PROBE/d' "$CADDY_FILE")"
  if echo "$other_blocks" | grep -Fq "$domain"; then
    echo -e "${RED}❌ 检测到域名 ${domain} 已被本机其他面板 (如 Nexus-Flux 或 NexusGate-Plus) 使用！请使用不同的二级域名。${RESET}"
    pause_return
    return
  fi

  # 仅更新 # BEGIN MY-VPS-PROBE 独立区块，绝不覆盖 Nexus-Flux 与其他面板配置
  sed -i '/# BEGIN MY-VPS-PROBE/,/# END MY-VPS-PROBE/d' "$CADDY_FILE"
  cat >> "$CADDY_FILE" <<EOF

# BEGIN MY-VPS-PROBE
${domain} {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}
# END MY-VPS-PROBE
EOF

  curl -fsS -X POST "http://127.0.0.1:8080/api/admin/local-op" \
    -H "Content-Type: application/json" \
    -H "X-Probe-Local: 1" \
    -d "{\"panel_domain\":\"${domain}\"}" >/dev/null 2>&1 || true

  systemctl enable caddy >/dev/null 2>&1 || true
  systemctl reload caddy 2>/dev/null || systemctl restart caddy 2>/dev/null || true

  echo -e "${GREEN}✅ 探针域名反代已配置成功：https://${domain}${RESET}"
  echo -e "💡 已采用 # BEGIN MY-VPS-PROBE 独立块写入，与本机 Nexus-Flux / NexusGate-Plus 共享 Caddy 零冲突！"
  pause_return
}

block_8080() {
  local domain
  domain="$(get_probe_domain)"
  if [ -z "$domain" ]; then
    echo -e "${YELLOW}⚠️ 尚未检测到绑定的探针域名！请先执行「4. 添加域名访问」绑定域名后再开启此项，以免无法访问面板。${RESET}"
    pause_return
    return
  fi

  # 1. 开启应用层 IP+8080 面板直连拦截（放行绑定域名与 Agent /ws 心跳上报）
  if curl -fsS -X POST "http://127.0.0.1:8080/api/admin/local-op" \
    -H "Content-Type: application/json" \
    -H "X-Probe-Local: 1" \
    -d "{\"panel_domain\":\"${domain}\",\"block_ip_access\":true}" >/dev/null 2>&1; then
    :
  else
    local repo
    repo="$(detect_repo || true)"
    if [ -n "$repo" ] && [ -f "$repo/config.json" ] && command -v python3 >/dev/null 2>&1; then
      python3 - "$repo/config.json" "$domain" <<'PY'
import json, sys
p, dom = sys.argv[1], sys.argv[2]
d = json.load(open(p, 'r', encoding='utf-8'))
d['panel_domain'] = dom
d['block_ip_access'] = True
json.dump(d, open(p, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
PY
      systemctl restart probe-server 2>/dev/null || true
    fi
  fi

  # 2. 若系统原本已启用 UFW，则自动保护其他面板端口后封锁 8080（绝不盲目强开 UFW 导致其他面板断连）
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow ssh >/dev/null 2>&1 || true
    ufw allow http >/dev/null 2>&1 || true
    ufw allow https >/dev/null 2>&1 || true
    if [ -f /opt/nexus-flux/.env ]; then
      local fl_port
      fl_port="$(grep '^FLUX_PORT=' /opt/nexus-flux/.env 2>/dev/null | cut -d= -f2 || echo 6366)"
      [ -n "$fl_port" ] && ufw allow "${fl_port}/tcp" >/dev/null 2>&1 || true
    fi
    ufw deny 8080/tcp >/dev/null 2>&1 || true
  fi

  echo -e "${GREEN}🔒 已阻止通过 IP+8080 直接访问探针面板！${RESET}"
  echo -e "   • 请通过绑定域名访问: ${CYAN}https://${domain}${RESET}"
  echo -e "   • 被控端 Agent 上报 (/ws) 与本机其他面板端口均不受影响。"
  pause_return
}

allow_8080() {
  curl -fsS -X POST "http://127.0.0.1:8080/api/admin/local-op" \
    -H "Content-Type: application/json" \
    -H "X-Probe-Local: 1" \
    -d '{"block_ip_access":false}' >/dev/null 2>&1 || true

  local repo
  repo="$(detect_repo || true)"
  if [ -n "$repo" ] && [ -f "$repo/config.json" ] && command -v python3 >/dev/null 2>&1; then
    python3 - "$repo/config.json" <<'PY' 2>/dev/null || true
import json, sys
p = sys.argv[1]
d = json.load(open(p, 'r', encoding='utf-8'))
d['block_ip_access'] = False
json.dump(d, open(p, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
PY
  fi

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow 8080 >/dev/null 2>&1 || true
    ufw reload >/dev/null 2>&1 || true
  fi

  echo -e "${GREEN}🔓 已允许通过 IP+8080 直接访问探针面板${RESET}"
  pause_return
}

uninstall_master() {
  read -r -p "确认卸载探针主控端? [y/N]: " ans </dev/tty || return
  [[ "$ans" =~ ^[Yy]$ ]] || return

  REPO_DIR="$(detect_repo || true)"

  systemctl stop probe-server 2>/dev/null || true
  systemctl disable probe-server 2>/dev/null || true
  rm -f /etc/systemd/system/probe-server.service
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true

  if [ -f "$CADDY_FILE" ]; then
    sed -i '/# BEGIN MY-VPS-PROBE/,/# END MY-VPS-PROBE/d' "$CADDY_FILE"
    systemctl reload caddy 2>/dev/null || true
  fi

  [ -n "$REPO_DIR" ] && rm -rf "$REPO_DIR"
  rm -f /usr/local/bin/tz /usr/bin/tz 2>/dev/null || true

  echo -e "${GREEN}✅ 探针主控端已卸载（不影响本机其他面板）${RESET}"
  exit 0
}

uninstall_agent() {
  read -r -p "确认卸载本机被控端? [y/N]: " ans </dev/tty || return
  [[ "$ans" =~ ^[Yy]$ ]] || return

  systemctl stop probe-agent 2>/dev/null || true
  systemctl disable probe-agent 2>/dev/null || true
  rm -f /etc/systemd/system/probe-agent.service
  rm -f /etc/probe/probe-agent
  rm -rf /etc/probe
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true

  echo -e "${GREEN}✅ 本机被控端已卸载${RESET}"
  pause_return
}

create_master_backup() {
  local repo_dir="$1"
  local prefix="${2:-my-vps-probe-backup}"
  local backup_dir="$repo_dir/backups"
  local stamp work_dir backup_file rc
  local files=(config.json data.db)

  stamp="$(date +%Y%m%d-%H%M%S)"
  work_dir="$(mktemp -d)"
  backup_file="$backup_dir/${prefix}-${stamp}.tar.gz"
  mkdir -p "$backup_dir"

  if [ ! -f "$repo_dir/config.json" ] || [ ! -f "$repo_dir/data.db" ]; then
    echo -e "${RED}❌ 找不到 config.json 或 data.db，无法备份${RESET}" >&2
    rm -rf "$work_dir"
    return 1
  fi

  # 优先尝试通过主控在线热备份接口导出（自动刷盘内存 Ping 与月流量状态，零停机）
  if systemctl is-active --quiet probe-server 2>/dev/null && command -v python3 >/dev/null 2>&1; then
    local admin_auth=""
    admin_auth="$(python3 - "$repo_dir/config.json" <<'PY' 2>/dev/null || true
import base64, json, sys
d = json.load(open(sys.argv[1], 'r', encoding='utf-8'))
u = d.get('admin_user') or 'admin'
p = d.get('admin_pass') or '123456'
print(base64.b64encode(f"{u}:{p}".encode()).decode())
PY
)"
    if [ -n "$admin_auth" ] && curl -fsSL -H "Authorization: Basic ${admin_auth}" "http://127.0.0.1:8080/api/admin/backup" -o "$backup_file" 2>/dev/null; then
      if [ -s "$backup_file" ] && tar -tzf "$backup_file" >/dev/null 2>&1; then
        rm -rf "$work_dir"
        chmod 600 "$backup_file"
        cp -f "$backup_file" /root/my-vps-probe-latest-backup.tar.gz 2>/dev/null || true
        [ -f "$CADDY_FILE" ] && cp -f "$CADDY_FILE" "$backup_dir/Caddyfile.bak" 2>/dev/null || true
        # 自动清理超出 15 份的历史旧备份
        ls -1t "$backup_dir"/my-vps-probe-backup-*.tar.gz 2>/dev/null | tail -n +16 | xargs -r rm -f -- 2>/dev/null || true
        printf '%s\n' "$backup_file"
        return 0
      fi
    fi
  fi

  # 兜底：使用 sqlite3 在线 .backup 或直接复制
  install -m 600 "$repo_dir/config.json" "$work_dir/config.json"
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$repo_dir/data.db" ".backup '$work_dir/data.db'"
    chmod 600 "$work_dir/data.db"
  else
    install -m 600 "$repo_dir/data.db" "$work_dir/data.db"
  fi

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
  chmod 600 "$backup_file"
  cp -f "$backup_file" /root/my-vps-probe-latest-backup.tar.gz 2>/dev/null || true
  [ -f "$CADDY_FILE" ] && cp -f "$CADDY_FILE" "$backup_dir/Caddyfile.bak" 2>/dev/null || true
  ls -1t "$backup_dir"/my-vps-probe-backup-*.tar.gz 2>/dev/null | tail -n +16 | xargs -r rm -f -- 2>/dev/null || true
  printf '%s\n' "$backup_file"
}

backup_master() {
  local repo_dir backup_file rc

  repo_dir="$(detect_repo || true)"
  [ -n "$repo_dir" ] || { echo -e "${RED}❌ 找不到主控仓库目录${RESET}"; pause_return; return; }

  echo -e "${CYAN}📦 正在执行零停机一致性全量热备份 (包含节点配置、月流量状态与历史数据库)...${RESET}"
  backup_file="$(create_master_backup "$repo_dir")" || rc=$?
  rc="${rc:-0}"

  if [ "$rc" -ne 0 ] || [ -z "$backup_file" ]; then
    echo -e "${RED}❌ 备份失败${RESET}"
    pause_return
    return
  fi

  echo -e "${GREEN}✅ 全量备份完成（主控服务全程在线未中断）！${RESET}"
  echo -e "  • 归档路径: ${YELLOW}${backup_file}${RESET}"
  echo -e "  • 快捷下载: ${CYAN}/root/my-vps-probe-latest-backup.tar.gz${RESET} (可在 FinalShell /root 目录直接下载)"
  ls -lh "$backup_file"
  pause_return
}

restore_master() {
  local input_arg="${1:-}"
  local repo_dir backup_file tmp_dir confirm protection_backup rc

  repo_dir="$(detect_repo || true)"
  [ -n "$repo_dir" ] || { echo -e "${RED}❌ 找不到主控仓库目录${RESET}"; pause_return; return; }

  if [ -n "$input_arg" ] && [ -f "$input_arg" ]; then
    backup_file="$input_arg"
  else
    echo -e "${CYAN}──────────────── 从备份恢复主控配置与历史数据 ────────────────${RESET}"
    local candidates=()
    while IFS= read -r f; do
      [ -n "$f" ] && candidates+=("$f")
    done < <(ls -1t /root/my-vps-probe*.tar.gz "$repo_dir/backups"/my-vps-probe-backup-*.tar.gz "$repo_dir/backups"/pre-*.tar.gz /tmp/my-vps-probe*.tar.gz 2>/dev/null | awk '!seen[$0]++' | head -n 15)

    if [ "${#candidates[@]}" -gt 0 ]; then
      echo -e "📂 检测到本机现有备份包列表："
      local i=1
      for c in "${candidates[@]}"; do
        local sz mtime
        sz="$(ls -lh "$c" 2>/dev/null | awk '{print $5}')"
        mtime="$(date -r "$c" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo '')"
        echo -e "  ${GREEN}[${i}]${RESET} ${c} ${YELLOW}(${sz}, ${mtime})${RESET}"
        i=$((i + 1))
      done
      echo ""
    fi
    read -r -p "请输入备份序号 [1-${#candidates[@]}] 或备份包完整路径 (直接回车取消): " user_pick </dev/tty || true
    user_pick="$(echo "$user_pick" | tr -d '[:space:]')"
    [ -n "$user_pick" ] || { echo "已取消恢复"; pause_return; return; }
    if [[ "$user_pick" =~ ^[0-9]+$ ]] && [ "$user_pick" -ge 1 ] && [ "$user_pick" -le "${#candidates[@]}" ]; then
      backup_file="${candidates[$((user_pick - 1))]}"
    else
      backup_file="$user_pick"
    fi
  fi

  [ -f "$backup_file" ] || { echo -e "${RED}❌ 备份文件不存在: $backup_file${RESET}"; pause_return; return; }

  tmp_dir="$(mktemp -d)"
  if ! tar -xzf "$backup_file" -C "$tmp_dir" 2>/dev/null; then
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 解压失败，请确认文件是有效的 .tar.gz 备份包${RESET}"
    pause_return
    return
  fi

  if [ ! -f "$tmp_dir/config.json" ] || ! grep -qx 'format=my-vps-probe-backup-v1' "$tmp_dir/manifest.txt" 2>/dev/null; then
    rm -rf "$tmp_dir"
    echo -e "${RED}❌ 备份清单缺失或格式不匹配，未恢复任何文件${RESET}"
    pause_return
    return
  fi

  if [ -f "$tmp_dir/SHA256SUMS" ]; then
    if ! (cd "$tmp_dir" && sha256sum -c SHA256SUMS >/dev/null 2>&1); then
      rm -rf "$tmp_dir"
      echo -e "${RED}❌ 备份文件 SHA256 完整性校验失败，已拒绝恢复${RESET}"
      pause_return
      return
    fi
  fi

  if [ -z "$input_arg" ]; then
    echo -e "${YELLOW}⚠️ 即将使用 [${backup_file}] 覆盖当前主控配置与历史数据！${RESET}"
    read -r -p "请输入 RESTORE 或 y 确认恢复: " confirm </dev/tty || true
    if [ "$confirm" != "RESTORE" ] && [ "$confirm" != "y" ] && [ "$confirm" != "Y" ]; then
      rm -rf "$tmp_dir"
      echo "已取消恢复"
      pause_return
      return
    fi
  fi

  echo -e "${YELLOW}🛡️ 正在创建恢复前保护备份...${RESET}"
  protection_backup="$(create_master_backup "$repo_dir" "pre-restore-guard" || true)"

  systemctl stop probe-server 2>/dev/null || true
  rm -f "$repo_dir/data.db-wal" "$repo_dir/data.db-shm" 2>/dev/null || true

  install -m 600 "$tmp_dir/config.json" "$repo_dir/config.json"
  [ -f "$tmp_dir/data.db" ] && install -m 600 "$tmp_dir/data.db" "$repo_dir/data.db"

  if [ -f "$tmp_dir/usage_state.json" ]; then
    install -m 600 "$tmp_dir/usage_state.json" "$repo_dir/usage_state.json"
  else
    rm -f "$repo_dir/usage_state.json"
  fi

  rm -rf "$tmp_dir"
  systemctl start probe-server
  sleep 2

  if systemctl is-active --quiet probe-server; then
    echo -e "${GREEN}✅ 备份恢复完成，主控服务已正常启动！${RESET}"
    [ -n "$protection_backup" ] && echo -e "  • 恢复前保护备份: ${YELLOW}${protection_backup}${RESET}"
  else
    echo -e "${RED}❌ 恢复后服务未能正常启动，正在自动回滚至恢复前保护备份...${RESET}"
    if [ -n "$protection_backup" ] && [ -f "$protection_backup" ]; then
      tar -xzf "$protection_backup" -C "$repo_dir" config.json data.db usage_state.json 2>/dev/null || true
      systemctl start probe-server 2>/dev/null || true
      echo -e "${GREEN}✅ 已自动回滚至恢复前状态${RESET}"
    fi
  fi
  pause_return
}

check_three_panels() {
  echo -e "${CYAN}======================================================================${RESET}"
  echo -e "${GREEN}       🔍 三面板共存冲突巡检与 456MB 低配机器优化中心 (SG 专版)       ${RESET}"
  echo -e "${CYAN}======================================================================${RESET}"

  local mem_info
  mem_info="$(free -m 2>/dev/null | awk '/^Mem:/{printf "已用 %d MB / 总计 %d MB (可用 %d MB)", $3, $2, $7}' || echo '未知')"
  local swap_info
  swap_info="$(free -m 2>/dev/null | awk '/^Swap:/{printf "已用 %d MB / 总计 %d MB", $3, $2}' || echo '未知')"
  echo -e " 🖥 系统物理内存 : ${YELLOW}${mem_info}${RESET}"
  echo -e " 💾 系统交换分区 : ${YELLOW}${swap_info}${RESET}"
  echo ""

  # 1. My VPS Probe
  local p_stat="${RED}未运行${RESET}"
  systemctl is-active --quiet probe-server 2>/dev/null && p_stat="${GREEN}运行中${RESET}"
  echo -e " 1️⃣ ${CYAN}极简私有探针 (tz)${RESET}"
  echo -e "    • 主控状态 : ${p_stat} | 监听端口: ${GREEN}8080${RESET} | 内存占用: ${YELLOW}$(get_proc_rss_mb 'probe-server')${RESET}"
  echo -e "    • 被控状态 : $(systemctl is-active --quiet probe-agent 2>/dev/null && echo -e "${GREEN}运行中${RESET}" || echo -e "${YELLOW}未运行${RESET}") | 内存占用: ${YELLOW}$(get_proc_rss_mb 'probe-agent')${RESET}"
  echo -e "    • 绑定域名 : ${YELLOW}$(get_probe_domain || echo '未绑定')${RESET} | 访问策略: $(get_probe_ip_lock)"

  # 2. Nexus-Flux
  local fl_stat="${YELLOW}未安装/未运行${RESET}"
  local fl_port="6366"
  [ -f /opt/nexus-flux/.env ] && fl_port="$(grep '^FLUX_PORT=' /opt/nexus-flux/.env 2>/dev/null | cut -d= -f2 || echo 6366)"
  systemctl is-active --quiet nexus-flux 2>/dev/null && fl_stat="${GREEN}运行中${RESET}"
  echo -e " 2️⃣ ${CYAN}Nexus-Flux 转发主控 (fl)${RESET}"
  echo -e "    • 服务状态 : ${fl_stat} | 监听端口: ${GREEN}${fl_port}${RESET} | 内存占用: ${YELLOW}$(get_proc_rss_mb 'nexus-flux/server.js')${RESET}"

  # 3. NexusGate-Plus
  local ng_stat="${YELLOW}未安装/未运行${RESET}"
  local ng_port="随机/未安装"
  if [ -f /var/lib/nexusgate/nexusgate.json ] && command -v node >/dev/null 2>&1; then
    ng_port="$(node -e "try{const d=JSON.parse(require('fs').readFileSync('/var/lib/nexusgate/nexusgate.json','utf8'));console.log(d.panel&&d.panel.port||'')}catch(e){}" 2>/dev/null || echo '未知')"
  fi
  systemctl is-active --quiet nexusgate 2>/dev/null && ng_stat="${GREEN}运行中${RESET}"
  echo -e " 3️⃣ ${CYAN}NexusGate-Plus 枢门集群 (ng)${RESET}"
  echo -e "    • 服务状态 : ${ng_stat} | 面板端口: ${GREEN}${ng_port}${RESET} | 内存占用: ${YELLOW}$(get_proc_rss_mb 'nexusgate/server.js')${RESET}"

  # 4. Caddy 反代共存状态
  local cd_stat="${YELLOW}未运行${RESET}"
  systemctl is-active --quiet caddy 2>/dev/null && cd_stat="${GREEN}运行中${RESET}"
  echo -e " 4️⃣ ${CYAN}Caddy 多面板共享 HTTPS 反代 (80/443)${RESET}"
  echo -e "    • 服务状态 : ${cd_stat} | 内存占用: ${YELLOW}$(get_proc_rss_mb 'caddy')${RESET}"
  if [ -f "$CADDY_FILE" ]; then
    local blocks
    blocks="$(grep -E '^[a-zA-Z0-9._-]+\s*\{' "$CADDY_FILE" 2>/dev/null | awk '{print $1}' | tr '\n' ' ' || true)"
    echo -e "    • 已托管域名: ${GREEN}${blocks:-无}${RESET}"
  fi

  echo -e "${CYAN}──────────────────────────────────────────────────────────────────────${RESET}"
  echo -e "  ${GREEN}1.${RESET} 一键优化三面板低内存限制与 Caddy 反代隔离（推荐 456MB/512MB 机器执行）"
  echo -e "  ${GREEN}2.${RESET} 为 NexusGate-Plus 添加 Caddy 域名反代（共享 443 自动 HTTPS，免 80 端口冲突）"
  echo -e "  ${GREEN}0.${RESET} 返回主菜单"
  read -r -p "请选择操作 [0-2]: " sub </dev/tty || return
  case "$sub" in
    1)
      local repo
      repo="$(detect_repo || true)"
      [ -n "$repo" ] && sync_systemd_low_mem "$repo" "$(get_service_bin)"
      migrate_caddyfile_probe_block
      # 限制 systemd-journald 日志内存占用
      mkdir -p /etc/systemd/journald.conf.d
      cat > /etc/systemd/journald.conf.d/99-lowmem-panels.conf <<'EOF'
[Journal]
SystemMaxUse=32M
SystemMaxFileSize=8M
RuntimeMaxUse=16M
MaxRetentionSec=5day
EOF
      systemctl restart systemd-journald 2>/dev/null || true
      journalctl --vacuum-size=16M >/dev/null 2>&1 || true
      # 若已安装 NexusGate-Plus 且本机内存 < 650MB，优化其 V8 老生代上限为 96MB
      local total_mem
      total_mem="$(free -m 2>/dev/null | awk '/^Mem:/{print $2}' || echo 1024)"
      if [ "${total_mem:-1024}" -lt 650 ] && [ -f /etc/systemd/system/nexusgate.service ]; then
        sed -i 's/--max-old-space-size=256/--max-old-space-size=96/g' /etc/systemd/system/nexusgate.service 2>/dev/null || true
        sed -i 's/MemoryMax=512M/MemoryMax=180M/g' /etc/systemd/system/nexusgate.service 2>/dev/null || true
        systemctl daemon-reload 2>/dev/null || true
      fi
      sync && echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true
      echo -e "${GREEN}✅ 三面板低内存配额、Caddy 独立块隔离与日志压缩优化已完成！${RESET}"
      pause_return
      ;;
    2)
      read -r -p "请输入要绑定到 NexusGate-Plus 的域名 (输入 clear 解绑): " ng_dom </dev/tty || true
      ng_dom="$(echo "$ng_dom" | tr -d '[:space:]' | sed -E 's#^https?://##; s#/.*$##')"
      [ -n "$ng_dom" ] || return
      mkdir -p /etc/caddy
      touch "$CADDY_FILE"
      sed -i '/# BEGIN NEXUSGATE-PLUS/,/# END NEXUSGATE-PLUS/d' "$CADDY_FILE"
      if [ "$ng_dom" != "clear" ]; then
        read -r -p "请输入 NexusGate-Plus 面板监听端口 [默认 ${ng_port}]: " input_ng_port </dev/tty || true
        local target_port="${input_ng_port:-$ng_port}"
        cat >> "$CADDY_FILE" <<EOF

# BEGIN NEXUSGATE-PLUS
${ng_dom} {
    encode zstd gzip
    reverse_proxy 127.0.0.1:${target_port} {
        transport http {
            tls_insecure_skip_verify
        }
    }
}
# END NEXUSGATE-PLUS
EOF
        systemctl reload caddy 2>/dev/null || systemctl restart caddy 2>/dev/null || true
        echo -e "${GREEN}✅ 已为 NexusGate-Plus 配置 Caddy HTTPS 反代: https://${ng_dom} -> 127.0.0.1:${target_port}${RESET}"
      else
        systemctl reload caddy 2>/dev/null || true
        echo -e "${GREEN}✅ 已移除 NexusGate-Plus 的 Caddy 反代配置${RESET}"
      fi
      pause_return
      ;;
  esac
}

show_menu() {
  local p_status="${RED}已停止${RESET}"
  systemctl is-active --quiet probe-server 2>/dev/null && p_status="${GREEN}运行中 ($(get_proc_rss_mb 'probe-server'))${RESET}"
  local dom
  dom="$(get_probe_domain)"
  local ip_lock
  ip_lock="$(get_probe_ip_lock)"

  echo ""
  echo -e "${CYAN}======================================================${RESET}"
  echo -e "${GREEN}          ✨ 极简私有探针 - 主控端管理面板 ✨         ${RESET}"
  echo -e "${CYAN}======================================================${RESET}"
  echo -e " 主控状态: ${p_status} | 域名: ${YELLOW}${dom:-未绑定}${RESET}"
  echo -e " 访问策略: ${CYAN}${ip_lock}${RESET}"
  echo -e "${CYAN}------------------------------------------------------${RESET}"
  echo -e " 1. 更新探针主控端 (支持本地 ZIP 包 / Git 无损热更新)"
  echo -e " 2. 重启探针主控端服务"
  echo -e " 3. 查看探针主控端状态"
  echo -e " 4. 添加域名访问 (自动配置 HTTPS 反代 · 三面板零冲突)"
  echo -e " 5. 阻止 IP+8080 端口直接访问"
  echo -e " 6. 允许 IP+8080 端口直接访问"
  echo -e " 7. 卸载探针主控端"
  echo -e " 8. 卸载本机被控端"
  echo -e " 9. 备份主控配置与历史数据 (零停机热备份 · 同步至 /root)"
  echo -e "10. 从备份恢复主控配置与历史数据 (支持列表序号一键选择)"
  echo -e "11. 三面板共存状态巡检与低内存优化 (探针 + Flux + NG)"
  echo -e " 0. 退出"
  echo -e "${CYAN}======================================================${RESET}"
}

if [[ $# -gt 0 ]]; then
  case "$1" in
    update) update_master "${2:-}" ;;
    restart) restart_master ;;
    status) status_master ;;
    domain) bind_domain ;;
    block-ip|block) block_8080 ;;
    allow-ip|allow) allow_8080 ;;
    backup) backup_master ;;
    restore) restore_master "${2:-}" ;;
    check|panels) check_three_panels ;;
    *) echo "用法: tz [update [zip路径]|restart|status|domain|block-ip|allow-ip|backup|restore [备份路径]|check]" ;;
  esac
  exit 0
fi

while true; do
  show_menu
  read -r -p "请输入你的选择 [0-11]: " choice </dev/tty || exit 0
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
    11) check_three_panels ;;
    0|q|exit)
      echo -e "${GREEN}👋 已退出探针管理面板${RESET}"
      exit 0
      ;;
    *)
      echo -e "${YELLOW}⚠️ 请输入有效选项 [0-11]${RESET}"
      sleep 1
      ;;
  esac
done
