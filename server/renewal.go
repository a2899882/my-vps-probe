package main

import (
	"log"
	"strings"
	"time"

	"my-vps-probe/common"
)

const (
	renewalMonthly   = "monthly"
	renewalQuarterly = "quarterly"
	renewalYearly    = "yearly"
)

func normalizeRenewalCycle(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "month", renewalMonthly:
		return renewalMonthly
	case "quarter", renewalQuarterly:
		return renewalQuarterly
	case "year", "annual", renewalYearly:
		return renewalYearly
	default:
		// Existing configurations predate this field. Annual is the least
		// surprising editor default, while AutoRenew remains disabled.
		return renewalYearly
	}
}

func renewalCycleDays(cycle string) int {
	switch normalizeRenewalCycle(cycle) {
	case renewalMonthly:
		return 30
	case renewalQuarterly:
		return 90
	default:
		return 365
	}
}

func replaceNodeExpireDate(raw, date string) string {
	parts := strings.Split(raw, "|")
	if len(parts) == 0 {
		return date
	}
	parts[0] = date
	return strings.Join(parts, "|")
}

func dateOrdinal(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// renewNode advances a due date only after its expiry day has ended. Durations
// are deliberately fixed at 30/90/365 days, matching the billing choices in
// the admin UI. Multiple missed periods are caught up in one operation.
func renewNode(node *common.NodeConfig, now time.Time) bool {
	if node == nil || !node.AutoRenew {
		return false
	}
	expiry, ok := parseNodeExpireDate(node.ExpireDate)
	if !ok {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if !expiry.Before(today) {
		return false
	}
	days := renewalCycleDays(node.RenewalCycle)
	elapsedDays := int(dateOrdinal(today).Sub(dateOrdinal(expiry)).Hours() / 24)
	periods := (elapsedDays + days - 1) / days
	if periods < 1 {
		periods = 1
	}
	next := expiry.AddDate(0, 0, periods*days)
	node.RenewalCycle = normalizeRenewalCycle(node.RenewalCycle)
	node.ExpireDate = replaceNodeExpireDate(node.ExpireDate, next.Format("2006/01/02"))
	return true
}

func renewNodes(nodes []common.NodeConfig, now time.Time) int {
	updated := 0
	for i := range nodes {
		if renewNode(&nodes[i], now) {
			updated++
		}
	}
	return updated
}

// renewDueNodes persists automatic renewals atomically. Callers can invoke it
// frequently; an unchanged configuration causes no disk write.
func renewDueNodes(now time.Time) int {
	configMutex.Lock()
	defer configMutex.Unlock()
	next := appConfig
	next.Nodes = append([]common.NodeConfig(nil), appConfig.Nodes...)
	updated := renewNodes(next.Nodes, now)
	if updated == 0 {
		return 0
	}
	if err := saveAppConfig(next); err != nil {
		log.Printf("自动延期配置写入失败: %v", err)
		return 0
	}
	appConfig = next
	log.Printf("已自动延期 %d 台节点", updated)
	return updated
}
