package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type cachedBinaryHash struct {
	modTime time.Time
	size    int64
	hash    string
}

var (
	agentHashCacheMu sync.Mutex
	agentHashCache   = make(map[string]cachedBinaryHash)

	agentUpdatePushMu   sync.Mutex
	agentUpdateLastPush = make(map[string]time.Time)
)

func normalizeAgentArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return ""
	}
}

func agentBinaryInfo(arch string) (downloadURL string, hash string) {
	norm := normalizeAgentArch(arch)
	if norm == "" {
		return "", ""
	}
	filePath := "server/probe-agent-" + norm
	info, err := os.Stat(filePath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", ""
	}
	agentHashCacheMu.Lock()
	if cached, ok := agentHashCache[norm]; ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		agentHashCacheMu.Unlock()
		return "/probe-agent-" + norm, cached.hash
	}
	agentHashCacheMu.Unlock()

	f, err := os.Open(filePath)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", ""
	}
	sum := hex.EncodeToString(h.Sum(nil))
	agentHashCacheMu.Lock()
	agentHashCache[norm] = cachedBinaryHash{modTime: info.ModTime(), size: info.Size(), hash: sum}
	agentHashCacheMu.Unlock()
	return "/probe-agent-" + norm, sum
}

func isAgentOutdated(st common.ServerStatus) bool {
	if !st.IsOnline || st.AgentArch == "" || st.AgentHash == "" {
		return false
	}
	_, latest := agentBinaryInfo(st.AgentArch)
	return latest != "" && !strings.EqualFold(st.AgentHash, latest)
}

func maybePushAgentAutoUpdate(nodeID, nodeName string, conn *agentConnection, st common.ServerStatus, tasks []common.PingTask, pingCfg common.PingConfig, rrCfg common.ReturnRouteConfig, reportSeconds int, autoUpdate bool) {
	if !autoUpdate || conn == nil || st.AgentArch == "" || st.AgentHash == "" {
		return
	}
	dlURL, latestHash := agentBinaryInfo(st.AgentArch)
	if dlURL == "" || latestHash == "" || strings.EqualFold(st.AgentHash, latestHash) {
		return
	}
	agentUpdatePushMu.Lock()
	if last, ok := agentUpdateLastPush[nodeID]; ok && time.Since(last) < 5*time.Minute {
		agentUpdatePushMu.Unlock()
		return
	}
	agentUpdateLastPush[nodeID] = time.Now()
	agentUpdatePushMu.Unlock()

	go func() {
		_ = conn.writeJSON(common.AgentInstruction{
			ServerName:       nodeName,
			PingTasks:        tasks,
			PingConfig:       pingCfg,
			ReturnRoute:      rrCfg,
			ReportSeconds:    reportSeconds,
			AutoUpdate:       true,
			AgentHash:        latestHash,
			AgentDownloadURL: dlURL,
		})
	}()
}

func adminTriggerAgentUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	agentUpdatePushMu.Lock()
	agentUpdateLastPush = make(map[string]time.Time)
	agentUpdatePushMu.Unlock()

	hotPushAllAgentsWithUpdate(true)
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}
