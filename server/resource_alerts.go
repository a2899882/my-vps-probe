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

func resourceValue(kind string, n common.NodeConfig, s common.ServerStatus, now time.Time) (float64, string, bool) {
	switch kind {
	case "cpu":
		return s.CPUUsage, fmt.Sprintf("%.1f%% / %d 核", s.CPUUsage, s.CPUCores), s.CPUCores > 0
	case "memory":
		return utilization(s.MemUsed, s.MemTotal)
	case "disk":
		return utilization(s.DiskUsed, s.DiskTotal)
	case "traffic":
		_, limit, day := parseNodeQuota(n.ExpireDate)
		if limit <= 0 {
			return 0, "", false
		}
		used := getMonthlyUsageForCycle(n.ID, n.ExpireDate, now)
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
	// Reading quota state happens before the monitor lock, avoiding a reverse lock order.
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
			// Five percentage points of hysteresis prevent flapping near the threshold.
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
