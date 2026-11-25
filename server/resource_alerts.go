package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type ResourceAlertRule struct {
	Enabled         bool    `json:"enabled"`
	Threshold       float64 `json:"threshold"`
	DurationSeconds int     `json:"duration_seconds"`
}

type ResourceAlertConfig struct {
	CPU     ResourceAlertRule `json:"cpu"`
	Memory  ResourceAlertRule `json:"memory"`
	Disk    ResourceAlertRule `json:"disk"`
	Traffic ResourceAlertRule `json:"traffic"`
}

type TCPBlockAlertConfig struct {
	Enabled         bool     `json:"enabled"`
	DurationSeconds int      `json:"duration_seconds"`
	MaxNotifyCount  int      `json:"max_notify_count"`
	RepeatMinutes   int      `json:"repeat_minutes"`
	NodeIDs         []string `json:"node_ids,omitempty"`
	ExcludedNodeIDs []string `json:"excluded_node_ids,omitempty"`
}

func (c *TCPBlockAlertConfig) normalize() {
	if !c.Enabled && c.DurationSeconds == 0 && c.MaxNotifyCount == 0 && c.RepeatMinutes == 0 {
		c.DurationSeconds = 180
		c.MaxNotifyCount = 3
		c.RepeatMinutes = 5
	}
	if c.DurationSeconds < 0 {
		c.DurationSeconds = 180
	}
	c.DurationSeconds = max(0, min(86400, c.DurationSeconds))
	if c.MaxNotifyCount < 0 {
		c.MaxNotifyCount = 3
	}
	c.MaxNotifyCount = min(100, c.MaxNotifyCount)
	if c.RepeatMinutes < 1 {
		c.RepeatMinutes = 5
	}
	c.RepeatMinutes = min(10080, c.RepeatMinutes)
	if c.ExcludedNodeIDs == nil {
		c.ExcludedNodeIDs = []string{}
	}
}

func (c TCPBlockAlertConfig) allowsNode(id string) bool {
	for _, ex := range c.ExcludedNodeIDs {
		if ex == id {
			return false
		}
	}
	if c.NodeIDs == nil {
		return true
	}
	for _, allowed := range c.NodeIDs {
		if allowed == id {
			return true
		}
	}
	return false
}

func (r *ResourceAlertRule) normalize(threshold float64, seconds int) {
	if r.Threshold == 0 {
		r.Threshold = threshold
		if r.DurationSeconds == 0 {
			r.DurationSeconds = seconds
		}
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold < 1 || r.Threshold > 100 {
		r.Threshold = threshold
	}
	r.DurationSeconds = max(0, min(86400, r.DurationSeconds))
}

func (r ResourceAlertConfig) rules() map[string]ResourceAlertRule {
	return map[string]ResourceAlertRule{"cpu": r.CPU, "memory": r.Memory, "disk": r.Disk, "traffic": r.Traffic}
}

var resourceNames = map[string]string{"cpu": "CPU 使用率", "memory": "内存使用率", "disk": "硬盘使用率", "traffic": "本月流量"}

type resourceAlertState struct {
	NodeID, Kind, Detail, Signature string
	Value                           float64
	Since, BelowSince, LastReport   time.Time
	Firing, Valid                   bool
}

type resourceAlertMonitor struct {
	sync.Mutex
	states map[string]resourceAlertState
}

var resourceMonitor = resourceAlertMonitor{states: make(map[string]resourceAlertState)}

func resourceKey(id, kind string) string { return "resource:" + kind + ":" + id }
func tcpBlockKey(id string) string       { return "tcp_block:" + id }

func (m *resourceAlertMonitor) restore(events map[string]time.Time) {
	m.Lock()
	defer m.Unlock()
	for key := range events {
		parts := strings.SplitN(key, ":", 3)
		if len(parts) == 3 && parts[0] == "resource" {
			m.states[key] = resourceAlertState{NodeID: parts[2], Kind: parts[1], Firing: true}
		}
	}
}
func (m *resourceAlertMonitor) snapshot() map[string]resourceAlertState {
	m.Lock()
	defer m.Unlock()
	out := make(map[string]resourceAlertState, len(m.states))
	for k, v := range m.states {
		out[k] = v
	}
	return out
}
func (m *resourceAlertMonitor) discard(key string) { m.Lock(); delete(m.states, key); m.Unlock() }

type tcpBlockAlertState struct {
	NodeID, Detail, RecoveryDetail, Signature string
	TargetCount, FailedCount                  int
	Since, BelowSince, LastReport             time.Time
	Firing, AllFailed, Valid                  bool
}

type tcpBlockAlertMonitor struct {
	sync.Mutex
	states map[string]tcpBlockAlertState
}

var tcpBlockMonitor = tcpBlockAlertMonitor{states: make(map[string]tcpBlockAlertState)}

func (m *tcpBlockAlertMonitor) restore(events map[string]time.Time) {
	m.Lock()
	defer m.Unlock()
	for key := range events {
		if strings.HasPrefix(key, "tcp_block:") {
			id := strings.TrimPrefix(key, "tcp_block:")
			if id != "" {
				m.states[key] = tcpBlockAlertState{NodeID: id, Firing: true}
			}
		}
	}
}

func (m *tcpBlockAlertMonitor) snapshot() map[string]tcpBlockAlertState {
	m.Lock()
	defer m.Unlock()
	out := make(map[string]tcpBlockAlertState, len(m.states))
	for k, v := range m.states {
		out[k] = v
	}
	return out
}

func (m *tcpBlockAlertMonitor) discard(key string) {
	m.Lock()
	delete(m.states, key)
	m.Unlock()
}

func evaluateTCPBlock(tasks []common.PingTask, st common.ServerStatus) (allFailed bool, failedCount int, targetCount int, failDetail string, okDetail string, valid bool) {
	if len(st.PingStatuses) == 0 {
		return false, 0, 0, "", "", false
	}
	byName := make(map[string]common.PingResult, len(st.PingStatuses))
	for _, p := range st.PingStatuses {
		byName[p.TargetName] = p
	}
	var v4Names []string
	var allNames []string
	if len(tasks) > 0 {
		for _, t := range tasks {
			if _, ok := byName[t.Name]; ok {
				allNames = append(allNames, t.Name)
				if common.NormalizeIPVersion(t.IPVersion) == "v4" {
					v4Names = append(v4Names, t.Name)
				}
			}
		}
		if len(allNames) == 0 {
			return false, 0, 0, "", "", false
		}
	} else {
		for _, p := range st.PingStatuses {
			allNames = append(allNames, p.TargetName)
			if common.NormalizeIPVersion(p.IPVersion) == "v4" {
				v4Names = append(v4Names, p.TargetName)
			}
		}
	}
	// Prefer IPv4 targets for three-network block detection when present, so
	// IPv4-only VPS nodes are not falsely flagged when IPv6 targets are added.
	checkNames := v4Names
	if len(checkNames) == 0 {
		checkNames = allNames
	}
	targetCount = len(checkNames)
	if targetCount == 0 {
		return false, 0, 0, "", "", false
	}
	failedNames := make([]string, 0, targetCount)
	okSummaries := make([]string, 0, targetCount)
	for _, name := range checkNames {
		res := byName[name]
		if math.IsNaN(res.CurrentDelay) || math.IsInf(res.CurrentDelay, 0) {
			return false, 0, 0, "", "", false
		}
		if res.CurrentDelay < 0 {
			failedNames = append(failedNames, name)
		} else {
			okSummaries = append(okSummaries, fmt.Sprintf("%s %.0fms", name, res.CurrentDelay))
		}
	}
	failedCount = len(failedNames)
	allFailed = failedCount == targetCount && targetCount > 0
	if allFailed {
		failDetail = fmt.Sprintf("%s（共 %d 个目标全部连接超时）", strings.Join(failedNames, " / "), targetCount)
	}
	if len(okSummaries) > 0 {
		okDetail = strings.Join(okSummaries, " · ")
	}
	return allFailed, failedCount, targetCount, failDetail, okDetail, true
}

func (m *tcpBlockAlertMonitor) observe(n common.NodeConfig, tasks []common.PingTask, st common.ServerStatus, tg TelegramConfig, seconds int, now time.Time) {
	allFailed, failedCount, targetCount, failDetail, okDetail, valid := evaluateTCPBlock(tasks, st)
	key := tcpBlockKey(n.ID)
	m.Lock()
	defer m.Unlock()
	state := m.states[key]
	signature := fmt.Sprintf("%d/%d", tg.TCPBlock.DurationSeconds, tg.RecoverySeconds)
	if state.Signature != signature || (!state.LastReport.IsZero() && now.Sub(state.LastReport) > reportFreshness(seconds)) {
		state.Since = time.Time{}
		state.BelowSince = time.Time{}
	}
	state.NodeID = n.ID
	state.Signature = signature
	state.Valid = valid
	state.AllFailed = allFailed
	state.FailedCount = failedCount
	state.TargetCount = targetCount
	if failDetail != "" {
		state.Detail = failDetail
	}
	if okDetail != "" {
		state.RecoveryDetail = okDetail
	}
	state.LastReport = now
	if !valid {
		state.Since = time.Time{}
		state.BelowSince = time.Time{}
		m.states[key] = state
		return
	}
	if allFailed {
		state.BelowSince = time.Time{}
		if state.Since.IsZero() {
			state.Since = now
		}
		if now.Sub(state.Since) >= time.Duration(tg.TCPBlock.DurationSeconds)*time.Second {
			state.Firing = true
		}
	} else {
		state.Since = time.Time{}
		if state.Firing {
			if state.BelowSince.IsZero() {
				state.BelowSince = now
			}
			if now.Sub(state.BelowSince) >= time.Duration(tg.RecoverySeconds)*time.Second {
				state.Firing = false
			}
		} else {
			state.BelowSince = time.Time{}
		}
	}
	m.states[key] = state
}

func isNodeTCPBlocked(nodeID string, tasks []common.PingTask, st common.ServerStatus, reportSeconds int, now time.Time) bool {
	if !statusIsFresh(st, reportSeconds, now) {
		return false
	}
	tcpBlockMonitor.Lock()
	state, ok := tcpBlockMonitor.states[tcpBlockKey(nodeID)]
	tcpBlockMonitor.Unlock()
	if ok && state.Valid && now.Sub(state.LastReport) <= reportFreshness(reportSeconds) && (state.Firing || state.AllFailed) {
		return true
	}
	allFailed, _, targetCount, _, _, valid := evaluateTCPBlock(tasks, st)
	return valid && targetCount >= 2 && allFailed
}

func resourceValue(kind string, n common.NodeConfig, s common.ServerStatus, now time.Time) (float64, string, bool) {
	switch kind {
	case "cpu":
		return s.CPUUsage, fmt.Sprintf("%.1f%% / %d 核", s.CPUUsage, s.CPUCores), s.CPUCores > 0
	case "memory":
		return utilization(s.MemUsed, s.MemTotal)
	case "disk":
		return utilization(s.DiskUsed, s.DiskTotal)
	case "traffic":
		if s.NetworkValid != nil && !*s.NetworkValid {
			return 0, "", false
		}
		_, limit, day := parseNodeQuota(n.ExpireDate)
		if limit <= 0 {
			return 0, "", false
		}
		usage := monthlyUsageSnapshot(n, now)
		if usage.Error {
			return 0, "", false
		}
		used := usage.Used
		value := float64(used) / float64(limit) / 1073741824 * 100
		return value, fmt.Sprintf("%.2f / %d GB，每月 %d 日重置", float64(used)/1073741824, limit, day), true
	}
	return 0, "", false
}
func utilization(used, total uint64) (float64, string, bool) {
	if total == 0 {
		return 0, "", false
	}
	return float64(used) / float64(total) * 100, fmt.Sprintf("%.2f / %.2f GiB", float64(used)/1073741824, float64(total)/1073741824), true
}

func (m *resourceAlertMonitor) observe(n common.NodeConfig, st common.ServerStatus, tg TelegramConfig, seconds int, now time.Time) {
	if !tg.ready() || tg.excludes(n.ID) {
		return
	}
	type sample struct {
		kind   string
		rule   ResourceAlertRule
		value  float64
		detail string
		valid  bool
	}
	samples := make([]sample, 0, 4)
	for kind, rule := range tg.Resources.rules() {
		if !rule.Enabled {
			continue
		}
		value, detail, valid := resourceValue(kind, n, st, now)
		samples = append(samples, sample{kind, rule, value, detail, valid && !math.IsNaN(value) && !math.IsInf(value, 0)})
	}
	m.Lock()
	defer m.Unlock()
	for _, s := range samples {
		key := resourceKey(n.ID, s.kind)
		state := m.states[key]
		signature := fmt.Sprintf("%g/%d/%d", s.rule.Threshold, s.rule.DurationSeconds, tg.RecoverySeconds)
		if state.Signature != signature || now.Sub(state.LastReport) > reportFreshness(seconds) {
			state.Since = time.Time{}
			state.BelowSince = time.Time{}
		}
		state.NodeID = n.ID
		state.Kind = s.kind
		state.Signature = signature
		state.Valid = s.valid
		state.Value = s.value
		state.Detail = s.detail
		state.LastReport = now
		if !s.valid {
			state.Since = time.Time{}
			state.BelowSince = time.Time{}
			m.states[key] = state
			continue
		}
		if s.value >= s.rule.Threshold {
			state.BelowSince = time.Time{}
			if state.Since.IsZero() {
				state.Since = now
			}
			if now.Sub(state.Since) >= time.Duration(s.rule.DurationSeconds)*time.Second {
				state.Firing = true
			}
		} else {
			state.Since = time.Time{}
			if state.Firing && s.value <= math.Max(0, s.rule.Threshold-5) {
				if state.BelowSince.IsZero() {
					state.BelowSince = now
				}
				if now.Sub(state.BelowSince) >= time.Duration(tg.RecoverySeconds)*time.Second {
					state.Firing = false
				}
			} else {
				state.BelowSince = time.Time{}
			}
		}
		m.states[key] = state
	}
}

func configuredNode(nodes []common.NodeConfig, id, token string) (common.NodeConfig, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, n.Token == token
		}
	}
	return common.NodeConfig{}, false
}
