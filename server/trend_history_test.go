package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"my-vps-probe/common"
)

func trendTestState(t *testing.T) {
	t.Helper()
	old := appConfig
	appConfig = validTestConfig()
	appConfig.PingTasks = []common.PingTask{{Name: "TCP"}}
	trendCache.Lock()
	trendCache.entries = make(map[string]trendCacheEntry)
	trendCache.Unlock()
	t.Cleanup(func() {
		appConfig = old
		trendCache.Lock()
		trendCache.entries = make(map[string]trendCacheEntry)
		trendCache.Unlock()
	})
}

func TestTrendHistoryBoundsPayloadAndPreservesPeaksAndArchive(t *testing.T) {
	database := notificationTestDB(t)
	trendTestState(t)
	_, err := database.Exec(`CREATE TABLE resource_history(timestamp DATETIME,server_id TEXT,cpu_usage REAL,mem_used INTEGER,mem_total INTEGER,disk_used INTEGER,disk_total INTEGER,swap_used INTEGER,swap_total INTEGER,load_1 REAL,net_in_speed INTEGER,net_out_speed INTEGER,tcp_connections INTEGER,udp_connections INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	resource, err := tx.Prepare(`INSERT INTO resource_history VALUES (?,'node-1',?,40,100,60,100,0,0,0.5,100,200,10,2)`)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close()
	ping, err := tx.Prepare(`INSERT INTO ping_history(timestamp,server_id,target_name,delay,loss_rate) VALUES (?,'node-1','TCP',0,25)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ping.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	for i := 1; i <= 10000; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		cpu := 12
		if i == 5000 {
			cpu = 99
		}
		if _, err := resource.Exec(ts, cpu); err != nil {
			t.Fatal(err)
		}
		if _, err := ping.Exec(ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"resource", "ping"} {
		w := httptest.NewRecorder()
		trendHistoryHandler(kind)(w, httptest.NewRequest("GET", "/api/"+kind+"_history?server_id=node-1&hours=168", nil))
		if w.Code != 200 {
			t.Fatalf("%s history: %d %s", kind, w.Code, w.Body.String())
		}
		var points []map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &points); err != nil {
			t.Fatal(err)
		}
		if len(points) > trendPointLimit || len(points) < 400 {
			t.Fatalf("unbounded/unexpected response: %s %d", kind, len(points))
		}
		peak := false
		for _, p := range points {
			if p["step_seconds"] != float64(trendStep(168)) {
				t.Fatal("missing resolution metadata")
			}
			if kind == "resource" && p["cpu_usage"] == float64(99) {
				peak = true
			}
			if kind == "ping" && (p["delay"] != float64(0) || p["loss"] != float64(25)) {
				t.Fatalf("zero-ms or partial loss corrupted: %+v", p)
			}
		}
		if kind == "resource" && !peak {
			t.Fatal("aggregation lost CPU spike")
		}
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + kind + "_history").Scan(&count); err != nil || count != 10000 {
			t.Fatal("archive modified")
		}
	}
	w := httptest.NewRecorder()
	trendHistoryHandler("resource")(w, httptest.NewRequest("GET", "/api/resource_history?server_id=node-1&hours=0.25", nil))
	var short []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &short); err != nil || len(short) == 0 || len(short) > 15 {
		t.Fatalf("realtime range incorrectly expands to 1h: %d %v", len(short), err)
	}
}

func TestTrendErrorsAreNotDisguisedAsMissingSamples(t *testing.T) {
	database := notificationTestDB(t)
	trendTestState(t)
	for _, tc := range []struct {
		method, query string
		want          int
	}{{"POST", "node-1", 405}, {"GET", "unknown", 404}} {
		w := httptest.NewRecorder()
		trendHistoryHandler("ping")(w, httptest.NewRequest(tc.method, "/api/ping_history?server_id="+tc.query, nil))
		if w.Code != tc.want {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	trendHistoryHandler("ping")(w, httptest.NewRequest("GET", "/api/ping_history?server_id=node-1&hours=1", nil).WithContext(ctx))
	if w.Code != 503 {
		t.Fatalf("cancelled query returned fabricated empty history: %d", w.Code)
	}
	database.Close()
	w = httptest.NewRecorder()
	trendHistoryHandler("ping")(w, httptest.NewRequest("GET", "/api/ping_history?server_id=node-1&hours=NaN", nil))
	if w.Code != 503 {
		t.Fatalf("database failure hidden: %d", w.Code)
	}
}

func TestTrendCacheHasMemoryAndEntryLimits(t *testing.T) {
	trendTestState(t)
	now := time.Now()
	for i := 0; i < 60; i++ {
		cacheTrend(fmt.Sprint(i), trendCacheEntry{at: now.Add(time.Duration(i)), data: make([]byte, 200000)})
	}
	total := 0
	for _, entry := range trendCache.entries {
		total += len(entry.data)
	}
	if total > 4<<20 || len(trendCache.entries) > 32 {
		t.Fatalf("cache grew unbounded: %d bytes, %d entries", total, len(trendCache.entries))
	}
	if _, ok := cachedTrend("59", now.Add(time.Minute)); ok {
		t.Fatal("expired history reused")
	}
}
