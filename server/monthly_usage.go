package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type monthlyUsageRecord struct {
	CycleKey  string `json:"cycle_key"`
	LastTotal uint64 `json:"last_total"`
	Used      uint64 `json:"used"`
	UpdatedAt int64  `json:"updated_at"`
}

type FrontendNode struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	ExpireDate     string              `json:"expire_date"`
	Region         string              `json:"region"`
	Group          string              `json:"group"`
	Status         common.ServerStatus `json:"status"`
	MonthUsed      uint64              `json:"month_used"`
	TrafficLimitGB int                 `json:"traffic_limit_gb"`
	ResetDay       int                 `json:"reset_day"`
}

var monthlyUsageMutex sync.Mutex
var monthlyUsageState map[string]monthlyUsageRecord
var monthlyUsageLoaded bool
var monthlyUsageDirty bool

func loadMonthlyUsageLocked() {
	if monthlyUsageLoaded {
		return
	}
	monthlyUsageLoaded = true
	monthlyUsageState = map[string]monthlyUsageRecord{}
	data, err := os.ReadFile("usage_state.json")
	if err == nil {
		_ = json.Unmarshal(data, &monthlyUsageState)
		if monthlyUsageState == nil {
			monthlyUsageState = map[string]monthlyUsageRecord{}
		}
	}
}

func saveMonthlyUsageLocked() error {
	if monthlyUsageState == nil {
		monthlyUsageState = map[string]monthlyUsageRecord{}
	}
	data, err := json.MarshalIndent(monthlyUsageState, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic("usage_state.json", data, 0600); err != nil {
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
		_ = saveMonthlyUsageLocked()
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
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()

	loadMonthlyUsageLocked()

	_, _, resetDay := parseNodeQuota(raw)
	total := inTransfer + outTransfer
	key := usageCycleKey(time.Now(), resetDay)
	rec := monthlyUsageState[nodeID]

	if rec.CycleKey == "" || rec.CycleKey != key {
		rec = monthlyUsageRecord{
			CycleKey:  key,
			LastTotal: total,
			Used:      0,
			UpdatedAt: time.Now().Unix(),
		}
	} else {
		if total >= rec.LastTotal {
			rec.Used += total - rec.LastTotal
		} else {
			// Network counters reset after a reboot. Preserve usage accumulated in
			// this billing cycle and continue from the new counter baseline.
			rec.LastTotal = total
		}
		rec.LastTotal = total
		rec.UpdatedAt = time.Now().Unix()
	}

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
	return FrontendNode{
		ID:             n.ID,
		Name:           n.Name,
		ExpireDate:     n.ExpireDate,
		Region:         n.Region,
		Group:          n.Group,
		Status:         st,
		MonthUsed:      getMonthlyUsage(n.ID),
		TrafficLimitGB: limitGB,
		ResetDay:       resetDay,
	}
}
