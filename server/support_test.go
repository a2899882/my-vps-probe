package main

import (
	"strings"
	"testing"
	"time"

	"my-vps-probe/common"
)

func validTestConfig() AppConfig {
	return AppConfig{
		SiteName: "Test", AdminUser: "admin", AdminPass: "password",
		Nodes:       []common.NodeConfig{{ID: "node-1", Name: "Node 1", Token: "token-123456"}},
		PingTasks:   []common.PingTask{{Name: "Ping", Host: "example.com:80"}},
		HistoryDays: 7, PublicRefreshSeconds: 3, AgentReportSeconds: 3,
	}
}

func TestValidateConfigRejectsDuplicateNodeTokens(t *testing.T) {
	config := validTestConfig()
	config.Nodes = append(config.Nodes, common.NodeConfig{ID: "node-2", Name: "Node 2", Token: "token-123456"})
	if err := validateConfig(&config); err == nil || !strings.Contains(err.Error(), "Token 重复") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateConfigNormalizesPerformanceSettings(t *testing.T) {
	config := validTestConfig()
	config.HistoryDays = 0
	config.PublicRefreshSeconds = 1
	config.AgentReportSeconds = 1000
	if err := validateConfig(&config); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	if config.HistoryDays != 7 || config.PublicRefreshSeconds != 3 || config.AgentReportSeconds != 3 {
		t.Fatalf("settings were not normalized: %+v", config)
	}
}

func TestMonthlyUsageSurvivesCounterReset(t *testing.T) {
	monthlyUsageMutex.Lock()
	monthlyUsageLoaded = true
	monthlyUsageDirty = false
	monthlyUsageState = map[string]monthlyUsageRecord{
		"node-1": {CycleKey: usageCycleKey(time.Now(), 1), LastTotal: 100, Used: 50},
	}
	monthlyUsageMutex.Unlock()

	updateMonthlyUsage("node-1", "2030/01/01|100|1", 100, 50)
	updateMonthlyUsage("node-1", "2030/01/01|100|1", 10, 10)

	monthlyUsageMutex.Lock()
	record := monthlyUsageState["node-1"]
	monthlyUsageMutex.Unlock()
	if record.Used != 100 {
		t.Fatalf("usage after reset = %d, want 100", record.Used)
	}
}
