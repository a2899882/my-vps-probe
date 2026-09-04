# My VPS Probe

轻量、自托管的 VPS 状态探针。单个 Go 主控 + 单个 Go 被控端，提供实时资源监控、TCP 延迟历史、月流量配额、到期管理、分组、Telegram 通知与迁移备份。

## 功能

- 明暗双主题监控页，支持卡片/精简表格视图、搜索、分组、状态筛选、排序和分页
- CPU、内存、Swap、磁盘、负载、上下行速率/流量、TCP/UDP 连接数与系统在线时长
- 资源历史和 TCP Ping 历史图表，Ping 目标可单独分配到指定节点
- 节点到期日、月流量上限和自定义重置日
- Telegram 离线、恢复和到期通知，事件自动去重
- 后台批量分组、批量删除、置顶/置底、移动到指定位置、永久应用排序、JSON 导入导出
- 后台可调历史保留天数、前台刷新间隔和 Agent 上报间隔
- `tz` 命令更新、状态、域名反代、防火墙、备份、恢复和卸载
- SQLite WAL、定时清理、延迟缓存与月流量定时原子落盘，适合低配 VPS

## 一键安装主控

支持 Debian/Ubuntu、systemd、amd64/arm64：

```bash
curl -fsSL https://raw.githubusercontent.com/a2899882/my-vps-probe/main/install-master.sh | bash
```

安装完成后访问 `http://服务器IP:8080`，后台地址为 `/admin`。首次默认账号为 `admin`、密码为 `123456`，请立即在“系统设置”中修改。

需要域名与自动 HTTPS 时，在 VPS 执行：

```bash
tz
```

选择“添加域名访问”，脚本会配置 Caddy 反向代理。

## 添加被控端

在后台“节点管理”中新建节点，点击该节点的“部署”图标复制命令，再到目标 VPS 执行。Agent 使用 systemd 常驻并自动重连，Token 存放在权限为 `0600` 的 `/etc/probe/agent.env`，不会显示在进程参数中。

## 更新

主控 VPS 执行 `tz`，选择 `1. 更新探针主控端`。更新过程会：

1. 拉取最新源码；
2. 在临时目录编译主控与 amd64/arm64 Agent；
3. 编译成功后原子替换文件并重启；
4. 通过 `/healthz` 健康检查确认服务可用。

新版 Agent 功能需要在对应节点重新执行一次后台生成的部署命令；旧 Agent 可继续连接。

> 从旧版首次升级到当前“纯源码轻量仓库”时，请先重新执行一次上面的主控一键安装命令。新版安装器会处理旧版已跟踪的二进制文件、保留 `config.json` 和数据库、安装新版 `tz` 菜单；以后继续使用 `tz` 更新即可。

## 备份与恢复

执行 `tz`：

- 选择 `9` 生成包含 `config.json`、`data.db`、月流量状态及校验文件的压缩包；
- 将备份上传到新 VPS，再选择 `10`，输入完整路径恢复；
- 恢复前会自动额外创建一份保护备份。

## 规模建议

默认设置适合几十台节点。达到 100 台以上时，建议把前台刷新和 Agent 上报间隔调整为 5 秒；历史数据通常保留 7–30 天即可。1 核 512MB–1GB 内存足以承载常规团队使用，数据库空间主要由“在线节点数 × Ping 目标数 × 保留天数”决定，系统会按设置每小时清理过期记录。

## 开发

```bash
go test ./...
go build -o probe-server ./server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server/probe-agent-amd64 ./agent
```

运行目录需要包含 `server/index.html`、`server/admin.html` 和 `install.sh`。运行状态文件会自动生成，均已加入 `.gitignore`。

## 目录

- `server/`：主控、Web UI、历史和通知
- `agent/`：被控端采集与并行 TCP 探测
- `common/`：主控与 Agent 共用协议
- `scripts/tz.sh`：主控管理菜单
- `install-master.sh`：主控一键安装
- `install.sh`：被控端一键安装
