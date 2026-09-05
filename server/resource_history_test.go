package main

import (
	"math"
	"testing"
	"time"

	"my-vps-probe/common"
)

func createResourceHistoryTable(t *testing.T) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE resource_history(
id INTEGER PRIMARY KEY AUTOINCREMENT,timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,server_id TEXT,
cpu_usage REAL,mem_used INTEGER,mem_total INTEGER,disk_used INTEGER,disk_total INTEGER,
swap_used INTEGER,swap_total INTEGER,load_1 REAL,net_in_speed INTEGER,net_out_speed INTEGER,
tcp_connections INTEGER,udp_connections INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeReportedStatusRepairsContainerMemoryUnderflow(t *testing.T) {
	previous := common.ServerStatus{MemUsed: 256, MemTotal: 1024}
	raw := common.ServerStatus{
		CPUUsage: math.NaN(), Load1: 0.2, MemUsed: ^uint64(0) - 239615999,
		MemTotal: 1024, NetInTransfer: ^uint64(0),
	}
	got, fields := sanitizeReportedStatus(raw, previous)
	if got.MemUsed != previous.MemUsed || got.CPUUsage != 0 || got.NetInTransfer != previous.NetInTransfer {
		t.Fatalf("status not repaired: %+v", got)
	}
	if len(fields) < 3 {
		t.Fatalf("corrections not reported: %v", fields)
	}
}

func TestBadNodeCannotStopAllResourceHistory(t *testing.T) {
	database := notificationTestDB(t)
	createResourceHistoryTable(t)
	now := time.Now()

	configMutex.Lock()
	oldConfig := appConfig
	appConfig = validTestConfig()
	appConfig.AgentReportSeconds = 3
	configMutex.Unlock()
	mapMutex.Lock()
	oldStatuses := serverStatusMap
	serverStatusMap = map[string]common.ServerStatus{
		"node-1": {IsOnline: true, LastReport: now.Unix(), CPUUsage: 12, MemUsed: 20, MemTotal: 100},
		"broken": {IsOnline: true, LastReport: now.Unix(), CPUUsage: 3, MemUsed: ^uint64(0) - 239615999, MemTotal: 100},
	}
	mapMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		appConfig = oldConfig
		configMutex.Unlock()
		mapMutex.Lock()
		serverStatusMap = oldStatuses
		mapMutex.Unlock()
	})

	result := saveHistoryToDBAt(now)
	if result.Err != nil || result.Attempted != 2 || result.Written != 2 {
		t.Fatalf("write result: %+v", result)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM resource_history`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history rows=%d err=%v", count, err)
	}
	var repaired int64
	if err := database.QueryRow(`SELECT mem_used FROM resource_history WHERE server_id='broken'`).Scan(&repaired); err != nil || repaired != 0 {
		t.Fatalf("bad memory value persisted: %d err=%v", repaired, err)
	}
}

func TestResourceHistoryMigrationClosesSchemaCursorAndAddsColumns(t *testing.T) {
	database := notificationTestDB(t)
	if _, err := database.Exec(`CREATE TABLE resource_history(id INTEGER PRIMARY KEY,timestamp DATETIME,server_id TEXT,cpu_usage REAL)`); err != nil {
		t.Fatal(err)
	}
	ensureResourceHistoryColumns()
	rows, err := database.Query(`PRAGMA table_info(resource_history)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["tcp_connections"] || !columns["udp_connections"] {
		t.Fatalf("migration columns: %v", columns)
	}
}
