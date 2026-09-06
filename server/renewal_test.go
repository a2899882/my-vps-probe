package main

import (
	"testing"
	"time"

	"my-vps-probe/common"
)

func TestRenewNodeUsesFixedCycleAndPreservesQuota(t *testing.T) {
	node := common.NodeConfig{
		ID:           "n1",
		ExpireDate:   "2026/09/07|400|19",
		RenewalCycle: renewalMonthly,
		AutoRenew:    true,
	}
	if renewNode(&node, time.Date(2026, 9, 7, 23, 59, 0, 0, time.Local)) {
		t.Fatal("expiry date was renewed before its day ended")
	}
	if !renewNode(&node, time.Date(2026, 9, 8, 0, 1, 0, 0, time.Local)) {
		t.Fatal("expired monthly node was not renewed")
	}
	if node.ExpireDate != "2026/10/07|400|19" {
		t.Fatalf("renewed configuration = %q", node.ExpireDate)
	}
}

func TestRenewNodeCatchesUpMultiplePeriods(t *testing.T) {
	tests := []struct {
		name  string
		cycle string
		want  string
	}{
		{name: "monthly", cycle: renewalMonthly, want: "2026/09/25|1000|1"},
		{name: "quarterly", cycle: renewalQuarterly, want: "2026/11/24|1000|1"},
		{name: "yearly", cycle: renewalYearly, want: "2027/06/02|1000|1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := common.NodeConfig{ExpireDate: "2025/06/02|1000|1", RenewalCycle: tt.cycle, AutoRenew: true}
			if !renewNode(&node, time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)) {
				t.Fatal("old expiry was not caught up")
			}
			if node.ExpireDate != tt.want {
				t.Fatalf("expiry = %q, want %q", node.ExpireDate, tt.want)
			}
			expiry, ok := parseNodeExpireDate(node.ExpireDate)
			if !ok || expiry.Before(time.Date(2026, 9, 6, 0, 0, 0, 0, time.Local)) {
				t.Fatalf("renewed expiry is still in the past: %v", expiry)
			}
		})
	}
}

func TestRenewNodeRequiresOptInAndValidDate(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 1, 0, 0, time.Local)
	manual := common.NodeConfig{ExpireDate: "2026/09/07|100|1", RenewalCycle: renewalMonthly}
	if renewNode(&manual, now) || manual.ExpireDate != "2026/09/07|100|1" {
		t.Fatal("manual renewal node was modified")
	}
	invalid := common.NodeConfig{ExpireDate: "长期|100|1", RenewalCycle: renewalMonthly, AutoRenew: true}
	if renewNode(&invalid, now) {
		t.Fatal("invalid date was renewed")
	}
	config := validTestConfig()
	config.Nodes[0] = common.NodeConfig{ID: "node-1", Name: "Node 1", Token: "token-123456", ExpireDate: "", RenewalCycle: renewalMonthly, AutoRenew: true}
	if err := validateConfig(&config); err == nil {
		t.Fatal("auto renewal without an expiry date passed validation")
	}
}

func TestFrontendNodeIncludesRenewalState(t *testing.T) {
	node := common.NodeConfig{ID: "n", ExpireDate: "2030/01/01|0|1", RenewalCycle: renewalQuarterly, AutoRenew: true}
	frontend := buildFrontendNode(node, common.ServerStatus{})
	if frontend.RenewalCycle != renewalQuarterly || !frontend.AutoRenew {
		t.Fatalf("renewal state missing from public node: %+v", frontend)
	}
}
