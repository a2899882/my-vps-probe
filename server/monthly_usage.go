package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type monthlyUsageRecord struct {
	CycleKey        string                       `json:"cycle_key"`
	LastTotal       uint64                       `json:"last_total"`
	Used            uint64                       `json:"used"`
	UpdatedAt       int64                        `json:"updated_at"`
	LastIn          uint64                       `json:"last_in"`
	LastOut         uint64                       `json:"last_out"`
	LastUptime      uint64                       `json:"last_uptime"`
	BootID          string                       `json:"boot_id,omitempty"`
	CountersReady   bool                         `json:"counters_ready"`
	BaselinePending bool                         `json:"baseline_pending,omitempty"`
	LastCounters    map[string]common.NetCounter `json:"last_counters,omitempty"`
	Revision        uint64                       `json:"revision"`
	CalibratedAt    int64                        `json:"calibrated_at,omitempty"`
	CalibratedUsed  uint64                       `json:"calibrated_used,omitempty"`
	CalibratedCycle string                       `json:"calibrated_cycle,omitempty"`
	PreviousUsed    uint64                       `json:"previous_used,omitempty"`
	Note            string                       `json:"note,omitempty"`
}

type FrontendNode struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	ExpireDate        string              `json:"expire_date"`
	RenewalCycle      string              `json:"renewal_cycle,omitempty"`
	AutoRenew         bool                `json:"auto_renew,omitempty"`
	Region            string              `json:"region"`
	Group             string              `json:"group"`
	Status            common.ServerStatus `json:"status"`
	MonthUsed         uint64              `json:"month_used"`
	MonthCalibratedAt int64               `json:"month_calibrated_at,omitempty"`
	MonthUsageError   bool                `json:"month_usage_error,omitempty"`
	TrafficLimitGB    int                 `json:"traffic_limit_gb"`
	ResetDay          int                 `json:"reset_day"`
}

var monthlyUsageMutex sync.Mutex
var monthlyUsageState map[string]monthlyUsageRecord
var monthlyUsageLoaded bool
var monthlyUsageDirty bool
var monthlyUsagePath = "usage_state.json"
var monthlyUsageLoadError error

func loadMonthlyUsageLocked() {
	if monthlyUsageLoaded {
		return
	}
	monthlyUsageLoaded = true
	monthlyUsageState = map[string]monthlyUsageRecord{}
	data, err := os.ReadFile(monthlyUsagePath)
	if err == nil {
		monthlyUsageLoadError = json.Unmarshal(data, &monthlyUsageState)
		if monthlyUsageState == nil {
			monthlyUsageState = map[string]monthlyUsageRecord{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		monthlyUsageLoadError = err
	}
	if monthlyUsageLoadError != nil {
		log.Printf("monthly state could not be loaded; preserving original file: %v", monthlyUsageLoadError)
	}
}

func saveMonthlyUsageLocked() error {
	if monthlyUsageLoadError != nil {
		return monthlyUsageLoadError
	}
	if monthlyUsageState == nil {
		monthlyUsageState = map[string]monthlyUsageRecord{}
	}
	data, err := json.MarshalIndent(monthlyUsageState, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(monthlyUsagePath, data, 0600); err != nil {
		return err
	}
	monthlyUsageDirty = false
	return nil
}

func flushMonthlyUsage() {
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()
	loadMonthlyUsageLocked()
	if monthlyUsageDirty {
		if err := saveMonthlyUsageLocked(); err != nil {
			log.Printf("flush monthly usage: %v", err)
		}
	}
}

func normalizeResetDay(day int) int {
	if day < 1 {
		return 1
	}
	if day > 28 {
		return 28
	}
	return day
}

func parseNodeQuota(raw string) (string, int, int) {
	expireDate := "2027/01/01"
	trafficLimitGB := 0
	resetDay := 1

	s := strings.TrimSpace(raw)
	if s == "" {
		return expireDate, trafficLimitGB, resetDay
	}

	parts := strings.Split(s, "|")
	if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
		expireDate = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil && v >= 0 {
			trafficLimitGB = v
		}
	}
	if len(parts) > 2 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil {
			resetDay = normalizeResetDay(v)
		}
	}

	return expireDate, trafficLimitGB, resetDay
}

func usageCycleKey(now time.Time, resetDay int) string {
	d := normalizeResetDay(resetDay)
	loc := now.Location()
	start := time.Date(now.Year(), now.Month(), d, 0, 0, 0, 0, loc)
	if now.Day() < d {
		prev := start.AddDate(0, -1, 0)
		start = time.Date(prev.Year(), prev.Month(), d, 0, 0, 0, 0, loc)
	}
	return start.Format("2006-01-02")
}

func updateMonthlyUsage(nodeID, raw string, inTransfer, outTransfer uint64) {
	observeMonthlyUsage(nodeID, raw, common.ServerStatus{NetInTransfer: inTransfer, NetOutTransfer: outTransfer}, time.Now())
}

func addUsage(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0 // Unknown reset: rebaseline instead of importing unverified history.
	}
	return current - previous
}

func observeMonthlyUsage(nodeID, raw string, status common.ServerStatus, now time.Time) {
	if status.NetworkValid != nil && !*status.NetworkValid {
		return
	}
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()

	loadMonthlyUsageLocked()
	if monthlyUsageLoadError != nil {
		return
	}

	_, _, resetDay := parseNodeQuota(raw)
	total := addUsage(status.NetInTransfer, status.NetOutTransfer)
	key := usageCycleKey(now, resetDay)
	rec := monthlyUsageState[nodeID]
	first := rec.CycleKey == "" || rec.BaselinePending
	newCycle := rec.CycleKey != key
	if newCycle {
		rec.Used = 0
		rec.Note = "新账期从首次有效上报建立基线"
	}
	if !first && !newCycle {
		var delta uint64
		reboot := rec.LastUptime > 0 && status.Uptime > 0 && status.Uptime < rec.LastUptime
		if rec.BootID != "" && status.BootID != "" {
			reboot = rec.BootID != status.BootID
		}
		switch {
		case reboot:
			// We know the new counters began during this same cycle. Include the
			// first report after reboot, even if it exceeds the old counter value.
			delta = total
			rec.Note = "已识别系统重启，保留账期用量并接续新计数"
		case !rec.CountersReady:
			delta = counterDelta(total, rec.LastTotal) // One-time legacy migration.
		case len(status.NetCounters) > 0 && len(rec.LastCounters) > 0:
			seen := make(map[string]bool, len(status.NetCounters))
			for _, counter := range status.NetCounters {
				if seen[counter.Name] {
					continue
				}
				seen[counter.Name] = true
				previous, exists := rec.LastCounters[counter.Name]
				if !exists {
					continue // A newly seen interface establishes its own baseline.
				}
				delta = addUsage(delta, addUsage(counterDelta(counter.In, previous.In), counterDelta(counter.Out, previous.Out)))
			}
		default:
			delta = addUsage(counterDelta(status.NetInTransfer, rec.LastIn), counterDelta(status.NetOutTransfer, rec.LastOut))
		}
		if !reboot && (status.NetInTransfer < rec.LastIn || status.NetOutTransfer < rec.LastOut) {
			rec.Note = "网卡计数发生变化，下降方向已重新建立基线；可按账单校准"
		}
		rec.Used = addUsage(rec.Used, delta)
	}
	if first && rec.CalibratedAt == 0 {
		rec.Note = "从接入探针开始累计；接入前的用量可手动补齐"
	}
	rec.LastCounters = nil
	if len(status.NetCounters) > 0 {
		rec.LastCounters = make(map[string]common.NetCounter, len(status.NetCounters))
		for _, counter := range status.NetCounters {
			rec.LastCounters[counter.Name] = counter
		}
	}
	rec.CycleKey, rec.LastTotal, rec.UpdatedAt = key, total, now.Unix()
	rec.LastIn, rec.LastOut, rec.LastUptime, rec.BootID = status.NetInTransfer, status.NetOutTransfer, status.Uptime, status.BootID
	rec.CountersReady, rec.BaselinePending = true, false
	monthlyUsageState[nodeID] = rec
	monthlyUsageDirty = true
}

func getMonthlyUsage(nodeID string) uint64 {
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()

	loadMonthlyUsageLocked()

	rec, ok := monthlyUsageState[nodeID]
	if !ok {
		return 0
	}
	return rec.Used
}

func buildFrontendNode(n common.NodeConfig, st common.ServerStatus) FrontendNode {
	_, limitGB, resetDay := parseNodeQuota(n.ExpireDate)
	usage := monthlyUsageSnapshot(n, time.Now())
	st.NetCounters = nil // Per-interface accounting state is not needed by the public cards.
	st.BootID = ""
	return FrontendNode{
		ID:                n.ID,
		Name:              n.Name,
		ExpireDate:        n.ExpireDate,
		RenewalCycle:      n.RenewalCycle,
		AutoRenew:         n.AutoRenew,
		Region:            n.Region,
		Group:             n.Group,
		Status:            st,
		MonthUsed:         usage.Used,
		MonthCalibratedAt: usage.CalibratedAt,
		MonthUsageError:   usage.Error,
		TrafficLimitGB:    limitGB,
		ResetDay:          resetDay,
	}
}

// An offline node also shows zero once its billing cycle has rolled over.
func getMonthlyUsageForCycle(id, raw string, now time.Time) uint64 {
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()
	loadMonthlyUsageLocked()
	_, _, day := parseNodeQuota(raw)
	rec := monthlyUsageState[id]
	if rec.CycleKey != usageCycleKey(now, day) {
		return 0
	}
	return rec.Used
}
