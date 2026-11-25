package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"my-vps-probe/common"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var validNodeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func stringsContext() context.Context {
	return context.Background()
}

type agentConnection struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *agentConnection) writeJSON(value interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(value)
}

func (c *agentConnection) close() error {
	return c.conn.Close()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func saveAppConfig(config AppConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic("config.json", data, 0600)
}

func trimLimited(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}

func normalizeGroupOrder(order []string, nodes []common.NodeConfig) []string {
	out := make([]string, 0, len(order)+8)
	seen := make(map[string]bool)
	for _, g := range order {
		g = trimLimited(g, 80)
		if g == "" || g == "未分组" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	for _, n := range nodes {
		g := trimLimited(n.Group, 80)
		if g == "" || g == "未分组" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

func validateConfig(config *AppConfig) error {
	normalizeAppConfig(config)
	config.SiteName = trimLimited(config.SiteName, 80)
	config.AdminUser = trimLimited(config.AdminUser, 80)
	config.PanelDomain = strings.ToLower(trimLimited(config.PanelDomain, 255))
	if config.SiteName == "" {
		return errors.New("站点名称不能为空")
	}
	if config.AdminUser == "" || config.AdminPass == "" {
		return errors.New("管理账号和密码不能为空")
	}
	if len(config.Nodes) > 5000 {
		return errors.New("节点数量超过 5000 台上限")
	}

	ids := make(map[string]bool, len(config.Nodes))
	tokens := make(map[string]bool, len(config.Nodes))
	for i := range config.Nodes {
		node := &config.Nodes[i]
		node.ID = trimLimited(node.ID, 120)
		node.Name = trimLimited(node.Name, 120)
		node.Token = strings.TrimSpace(node.Token)
		node.Group = trimLimited(node.Group, 80)
		node.Region = trimLimited(node.Region, 16)
		node.ExpireDate = trimLimited(node.ExpireDate, 80)
		node.RenewalCycle = normalizeRenewalCycle(node.RenewalCycle)
		if node.ID == "" || !validNodeID.MatchString(node.ID) {
			return fmt.Errorf("第 %d 个节点 ID 无效，只能使用字母、数字、点、下划线和短横线", i+1)
		}
		if node.Name == "" {
			return fmt.Errorf("第 %d 个节点名称不能为空", i+1)
		}
		if strings.ContainsAny(node.Token, "\r\n") {
			return fmt.Errorf("节点“%s”的 Token 不能包含换行符", node.Name)
		}
		if len(node.Token) < 8 || len(node.Token) > 256 {
			return fmt.Errorf("节点“%s”的 Token 长度需为 8–256 个字符", node.Name)
		}
		if node.AutoRenew {
			if _, ok := parseNodeExpireDate(node.ExpireDate); !ok {
				return fmt.Errorf("节点“%s”启用自动延期前需要填写有效到期日期", node.Name)
			}
		}
		if ids[node.ID] {
			return fmt.Errorf("节点 ID 重复：%s", node.ID)
		}
		if tokens[node.Token] {
			return fmt.Errorf("节点 Token 重复：%s", node.Name)
		}
		ids[node.ID] = true
		tokens[node.Token] = true
	}

	config.GroupOrder = normalizeGroupOrder(config.GroupOrder, config.Nodes)

	excluded := make([]string, 0, len(config.Telegram.ExcludedNodeIDs))
	seenExcluded := map[string]bool{}
	for _, id := range config.Telegram.ExcludedNodeIDs {
		if ids[id] && !seenExcluded[id] {
			excluded = append(excluded, id)
			seenExcluded[id] = true
		}
	}
	config.Telegram.ExcludedNodeIDs = excluded

	if config.Telegram.TCPBlock.NodeIDs != nil {
		tcpSelected := make([]string, 0, len(config.Telegram.TCPBlock.NodeIDs))
		seenTCP := map[string]bool{}
		for _, id := range config.Telegram.TCPBlock.NodeIDs {
			if ids[id] && !seenTCP[id] {
				tcpSelected = append(tcpSelected, id)
				seenTCP[id] = true
			}
		}
		config.Telegram.TCPBlock.NodeIDs = tcpSelected
	}
	tcpExcluded := make([]string, 0, len(config.Telegram.TCPBlock.ExcludedNodeIDs))
	seenTCPEx := map[string]bool{}
	for _, id := range config.Telegram.TCPBlock.ExcludedNodeIDs {
		if ids[id] && !seenTCPEx[id] {
			tcpExcluded = append(tcpExcluded, id)
			seenTCPEx[id] = true
		}
	}
	config.Telegram.TCPBlock.ExcludedNodeIDs = tcpExcluded

	if config.Telegram.Enabled && (config.Telegram.Token == "" || config.Telegram.ChatID == "") {
		return errors.New("启用 Telegram 通知需要填写 Bot Token 和 Chat ID")
	}
	pingNames := make(map[string]bool, len(config.PingTasks))
	for i := range config.PingTasks {
		task := &config.PingTasks[i]
		task.Name = trimLimited(task.Name, 80)
		task.Host = trimLimited(task.Host, 255)
		task.IPVersion = common.NormalizeIPVersion(task.IPVersion)
		if task.Name == "" || task.Host == "" {
			return fmt.Errorf("第 %d 个 Ping 目标的名称和地址不能为空", i+1)
		}
		if pingNames[task.Name] {
			return fmt.Errorf("Ping 目标名称重复：%s", task.Name)
		}
		pingNames[task.Name] = true
		if task.NodeIDs != nil {
			selected := make([]string, 0, len(task.NodeIDs))
			seen := map[string]bool{}
			for _, id := range task.NodeIDs {
				if ids[id] && !seen[id] {
					selected = append(selected, id)
					seen[id] = true
				}
			}
			task.NodeIDs = selected
		}
	}
	for i := range config.ReturnRoute.Targets {
		t := &config.ReturnRoute.Targets[i]
		if t.NodeIDs != nil {
			selected := make([]string, 0, len(t.NodeIDs))
			seen := map[string]bool{}
			for _, id := range t.NodeIDs {
				if ids[id] && !seen[id] {
					selected = append(selected, id)
					seen[id] = true
				}
			}
			t.NodeIDs = selected
		}
	}
	return nil
}

func hotPushAllAgents() {
	configMutex.RLock()
	pTasks := append([]common.PingTask(nil), appConfig.PingTasks...)
	pingCfg := appConfig.Ping
	rrCfg := appConfig.ReturnRoute
	reportSeconds := appConfig.AgentReportSeconds
	nodeNameMap := make(map[string]string, len(appConfig.Nodes))
	for _, n := range appConfig.Nodes {
		nodeNameMap[n.ID] = n.Name
	}
	configMutex.RUnlock()

	mapMutex.Lock()
	for id := range serverStatusMap {
		if _, exists := nodeNameMap[id]; !exists {
			delete(serverStatusMap, id)
		}
	}
	mapMutex.Unlock()
	invalidateCardPingCache()

	connMutex.Lock()
	connections := make(map[string]*agentConnection, len(activeConns))
	for id, conn := range activeConns {
		connections[id] = conn
	}
	connMutex.Unlock()
	var pushWG sync.WaitGroup
	pushLimit := make(chan struct{}, 16)
	for id, conn := range connections {
		pushWG.Add(1)
		go func(id string, conn *agentConnection) {
			defer pushWG.Done()
			pushLimit <- struct{}{}
			defer func() { <-pushLimit }()
			name, ok := nodeNameMap[id]
			if !ok {
				removeActiveConnection(id, conn)
				_ = conn.close()
				return
			}
			instruction := common.AgentInstruction{
				ServerName:    name,
				PingTasks:     activePingTasksForNode(pTasks, pingCfg, id),
				PingConfig:    pingCfg,
				ReturnRoute:   returnRouteForNode(rrCfg, id),
				ReportSeconds: reportSeconds,
			}
			if err := conn.writeJSON(instruction); err != nil {
				removeActiveConnection(id, conn)
				_ = conn.close()
				markNodeOffline(id)
			}
		}(id, conn)
	}
	pushWG.Wait()
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ipAccessGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/ws" || path == "/healthz" || path == "/install.sh" ||
			path == "/probe-agent-amd64" || path == "/probe-agent-arm64" || path == "/download/agent.go" {
			next.ServeHTTP(w, r)
			return
		}
		configMutex.RLock()
		blockIP := appConfig.BlockIPAccess
		domain := strings.ToLower(strings.TrimSpace(appConfig.PanelDomain))
		configMutex.RUnlock()
		if !blockIP {
			next.ServeHTTP(w, r)
			return
		}
		reqHost := strings.ToLower(strings.TrimSpace(r.Host))
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = strings.Trim(h, "[]")
		}
		if domain != "" && strings.EqualFold(reqHost, domain) {
			next.ServeHTTP(w, r)
			return
		}
		if isLoopbackAddr(r.RemoteAddr) && (reqHost == "127.0.0.1" || reqHost == "localhost" || reqHost == "::1") && r.Header.Get("X-Forwarded-Host") == "" && r.Header.Get("X-Real-IP") == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("403 Forbidden: Direct IP+Port access is disabled. Please access via bound HTTPS domain."))
	})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w gzipResponseWriter) Write(data []byte) (int, error) {
	w.Header().Del("Content-Length")
	return w.writer.Write(data)
}

func gzipTextResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/backup" {
			next.ServeHTTP(w, r)
			return
		}
		compressible := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/" || r.URL.Path == "/admin"
		if !compressible || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(gzipResponseWriter{ResponseWriter: w, writer: gz}, r)
	})
}

func markNodeOffline(id string) {
	mapMutex.Lock()
	status := serverStatusMap[id]
	status.IsOnline = false
	serverStatusMap[id] = status
	mapMutex.Unlock()
}

func removeActiveConnection(id string, target *agentConnection) {
	connMutex.Lock()
	if activeConns[id] == target {
		delete(activeConns, id)
	}
	connMutex.Unlock()
}

type nodeRuntime struct {
	Online     bool    `json:"online"`
	TCPBlocked bool    `json:"tcp_blocked,omitempty"`
	LastReport int64   `json:"last_report"`
	CPUUsage   float64 `json:"cpu_usage"`
	MemUsed    uint64  `json:"mem_used"`
	MemTotal   uint64  `json:"mem_total"`
	DiskUsed   uint64  `json:"disk_used"`
	DiskTotal  uint64  `json:"disk_total"`
}

func adminRuntimeSnapshot() map[string]interface{} {
	configMutex.RLock()
	reportSeconds := appConfig.AgentReportSeconds
	nodeConfigs := append([]common.NodeConfig(nil), appConfig.Nodes...)
	pingTasks := append([]common.PingTask(nil), appConfig.PingTasks...)
	pingCfg := appConfig.Ping
	configMutex.RUnlock()
	mapMutex.RLock()
	runtimeNodes := make(map[string]nodeRuntime, len(serverStatusMap))
	online := 0
	tcpBlockedCount := 0
	now := time.Now()
	for id, status := range serverStatusMap {
		status.IsOnline = statusIsFresh(status, reportSeconds, now)
		if status.IsOnline {
			online++
		}
		blocked := isNodeTCPBlocked(id, activePingTasksForNode(pingTasks, pingCfg, id), status, reportSeconds, now)
		if blocked {
			tcpBlockedCount++
		}
		runtimeNodes[id] = nodeRuntime{
			Online: status.IsOnline, TCPBlocked: blocked, LastReport: status.LastReport, CPUUsage: status.CPUUsage,
			MemUsed: status.MemUsed, MemTotal: status.MemTotal,
			DiskUsed: status.DiskUsed, DiskTotal: status.DiskTotal,
		}
	}
	mapMutex.RUnlock()
	traffic := make(map[string]trafficUsageView, len(nodeConfigs))
	for _, n := range nodeConfigs {
		traffic[n.ID] = monthlyUsageSnapshot(n, now)
	}

	configMutex.RLock()
	total := len(appConfig.Nodes)
	historyDays := appConfig.HistoryDays
	configMutex.RUnlock()

	var dbBytes int64
	for _, path := range []string{"data.db", "data.db-wal", "data.db-shm"} {
		if info, err := os.Stat(path); err == nil {
			dbBytes += info.Size()
		}
	}
	return map[string]interface{}{
		"updated_at": now.Unix(), "online": online, "total": total, "tcp_blocked": tcpBlockedCount,
		"database_bytes": dbBytes, "history_days": historyDays, "nodes": runtimeNodes,
		"traffic": traffic, "resource_history": resourceHistorySnapshot(),
	}
}
