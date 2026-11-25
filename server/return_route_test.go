package main

import (
	"testing"
	"time"

	"my-vps-probe/common"
)

func TestReturnRouteMonitorDebounceAndAlert(t *testing.T) {
	m := returnRouteStateMonitor{states: make(map[string]*routeTrackState)}
	node := common.NodeConfig{ID: "us-1", Name: "US GIA", Group: "US优化"}
	cfg := common.ReturnRouteConfig{Enabled: true, IntervalMinutes: 30, NotifyChange: true, ConfirmCount: 2}
	now := time.Unix(1700000000, 0)

	// Initial baseline
	m.observe(node, []common.ReturnRouteResult{{Carrier: "CT", Label: "电信回程", IPVersion: "v4", Line: "CN2 GIA", CheckedAt: 100}}, cfg, now)
	if alerts := m.drainAlerts(); len(alerts) != 0 {
		t.Fatalf("baseline should not alert, got %+v", alerts)
	}

	// Single transient change should not trigger yet
	m.observe(node, []common.ReturnRouteResult{{Carrier: "CT", Label: "电信回程", IPVersion: "v4", Line: "163", CheckedAt: 200}}, cfg, now.Add(time.Minute))
	if alerts := m.drainAlerts(); len(alerts) != 0 {
		t.Fatalf("unconfirmed single change should not alert, got %+v", alerts)
	}

	// Second consecutive observation confirms change and queues alert
	m.observe(node, []common.ReturnRouteResult{{Carrier: "CT", Label: "电信回程", IPVersion: "v4", Line: "163", CheckedAt: 300}}, cfg, now.Add(2*time.Minute))
	alerts := m.drainAlerts()
	if len(alerts) != 1 || alerts[0].OldLine != "CN2 GIA" || alerts[0].NewLine != "163" {
		t.Fatalf("expected confirmed route change alert, got %+v", alerts)
	}

	enriched := m.enrich("us-1", []common.ReturnRouteResult{{Carrier: "CT", Label: "电信回程", IPVersion: "v4", Line: "163", CheckedAt: 300}})
	if len(enriched) != 1 || enriched[0].PrevLine != "CN2 GIA" || enriched[0].ChangedAt == 0 {
		t.Fatalf("expected enriched prev_line and changed_at, got %+v", enriched)
	}
}
