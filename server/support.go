package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"my-vps-probe/common"
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

func validateConfig(config *AppConfig) error {
	normalizeAppConfig(config)
	config.SiteName = trimLimited(config.SiteName, 80)
	config.AdminUser = trimLimited(config.AdminUser, 80)
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
		if ids[node.ID] {
			return fmt.Errorf("节点 ID 重复：%s", node.ID)
		}
		if tokens[node.Token] {
			return fmt.Errorf("节点 Token 重复：%s", node.Name)
		}
		ids[node.ID] = true
		tokens[node.Token] = true
	}

	excluded := make([]string, 0, len(config.Telegram.ExcludedNodeIDs))
	seenExcluded := map[string]bool{}
	for _, id := range config.Telegram.ExcludedNodeIDs {
		if ids[id] && !seenExcluded[id] {
			excluded = append(excluded, id)
			seenExcluded[id] = true
		}
	}
	config.Telegram.ExcludedNodeIDs = excluded
	if config.Telegram.Enabled && (config.Telegram.Token == "" || config.Telegram.ChatID == "") {
		return errors.New("启用 Telegram 通知需要填写 Bot Token 和 Chat ID")
	}
	pingNames := make(map[string]bool, len(config.PingTasks))
	for i := range config.PingTasks {
		task := &config.PingTasks[i]
		task.Name = trimLimited(task.Name, 80)
		task.Host = trimLimited(task.Host, 255)
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
	return nil
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
	configMutex.RUnlock()
	mapMutex.RLock()
	runtimeNodes := make(map[string]nodeRuntime, len(serverStatusMap))
	online := 0
	for id, status := range serverStatusMap {
		status.IsOnline = statusIsFresh(status, reportSeconds, time.Now())
		if status.IsOnline {
			online++
		}
		runtimeNodes[id] = nodeRuntime{
			Online: status.IsOnline, LastReport: status.LastReport, CPUUsage: status.CPUUsage,
			MemUsed: status.MemUsed, MemTotal: status.MemTotal,
			DiskUsed: status.DiskUsed, DiskTotal: status.DiskTotal,
		}
	}
	mapMutex.RUnlock()
	traffic := make(map[string]trafficUsageView, len(nodeConfigs))
	for _, n := range nodeConfigs {
		traffic[n.ID] = monthlyUsageSnapshot(n, time.Now())
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
		"updated_at": time.Now().Unix(), "online": online, "total": total,
		"database_bytes": dbBytes, "history_days": historyDays, "nodes": runtimeNodes,
		"traffic": traffic,
	}
}
