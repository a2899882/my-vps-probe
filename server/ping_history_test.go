package main

import (
	"database/sql"
	"my-vps-probe/common"
	"path/filepath"
	"testing"
	"time"
)

func notificationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	old := db
	db = database
	for _, q := range []string{
		`CREATE TABLE ping_history(id INTEGER PRIMARY KEY,timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,server_id TEXT,target_name TEXT,delay REAL,loss_rate REAL)`,
		`CREATE TABLE notification_events(event_key TEXT PRIMARY KEY,sent_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE notification_log(id INTEGER PRIMARY KEY,created_at DATETIME DEFAULT CURRENT_TIMESTAMP,node_id TEXT,kind TEXT,status TEXT,message TEXT,error TEXT)`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	pingBuffer.Lock()
	pingBuffer.items = make(map[pingMinuteKey]pingMinute)
	pingBuffer.Unlock()
	invalidateCardPingCache()
	t.Cleanup(func() {
		db = old
		database.Close()
		pingBuffer.Lock()
		pingBuffer.items = make(map[pingMinuteKey]pingMinute)
		pingBuffer.Unlock()
		invalidateCardPingCache()
	})
	return database
}

func TestPingMinutesSurviveDelayedWritesAndTimezones(t *testing.T) {
	database := notificationTestDB(t)
	end := time.Date(2026, 9, 5, 13, 0, 30, 0, time.FixedZone("UTC+8", 8*3600))
	tasks := []common.PingTask{{Name: "TCP"}}
	for i := 59; i >= 0; i-- {
		recordPingSamples("n", []common.PingResult{{TargetName: "TCP", CurrentDelay: 12}}, end.Add(-time.Duration(i)*time.Minute))
	}
	st := common.ServerStatus{IsOnline: true, LastReport: end.Unix(), PingStatuses: []common.PingResult{{TargetName: "TCP", CurrentDelay: 12}}}
	before := cardPingStatuses("n", tasks, st, 3, end)
	if before[0].SampleMinutes != 60 || before[0].History60[59] == nil || !before[0].HasCurrent {
		t.Fatalf("unflushed samples missing: %+v", before[0])
	}
	flushPingMinutes(end, false)
	var count int
	database.QueryRow(`SELECT COUNT(*) FROM ping_history`).Scan(&count)
	if count != 59 {
		t.Fatalf("closed minutes=%d, want 59", count)
	}
	points, err := readCardPingHistory(database, "n", end)
	if err != nil {
		t.Fatal(err)
	}
	after := cardPingStatuses("n", tasks, st, 3, end)
	if len(points) != 59 || after[0].SampleMinutes != 60 {
		t.Fatalf("cached/database samples not aligned: %d / %d", len(points), after[0].SampleMinutes)
	}
	for i, v := range after[0].History60 {
		if v == nil || *v != 12 {
			t.Fatalf("gap at %d", i)
		}
	}
	if after[0].HistoryStart != (end.Unix()/60-59)*60 {
		t.Fatal("window must use UTC epoch minutes")
	}
	// The same instant in UTC must produce an identical window.
	utc := buildCardPingStatuses("n", tasks, points, pendingPingMinutes(), st, 3, end.UTC())
	if utc[0].SampleMinutes != 60 || utc[0].HistoryStart != after[0].HistoryStart {
		t.Fatal("timezone changed sample coverage")
	}
}

func TestPingDatabaseFailureRetriesWithoutInventingData(t *testing.T) {
	database := notificationTestDB(t)
	now := time.Date(2026, 9, 5, 5, 10, 30, 0, time.UTC)
	recordPingSamples("n", []common.PingResult{{TargetName: "TCP", CurrentDelay: -1}}, now.Add(-time.Minute))
	recordPingSamples("n", []common.PingResult{{TargetName: "TCP", CurrentDelay: 20}}, now.Add(-time.Minute))
	database.Exec(`CREATE TRIGGER fail_write BEFORE INSERT ON ping_history BEGIN SELECT RAISE(FAIL,'test write failure'); END`)
	flushPingMinutes(now, false)
	if len(pendingPingMinutes()) != 1 {
		t.Fatal("failed write lost pending samples")
	}
	database.Exec(`DROP TRIGGER fail_write`)
	flushPingMinutes(now, false)
	if len(pendingPingMinutes()) != 0 {
		t.Fatal("successful retry did not clear buffer")
	}
	p := cardPingStatuses("n", []common.PingTask{{Name: "TCP"}}, common.ServerStatus{}, 3, now)[0]
	if p.SampleMinutes != 1 || p.History60[58] == nil || *p.History60[58] != 20 || *p.HistoryLoss60[58] != 50 {
		t.Fatalf("partial failure not preserved: %+v", p)
	}
	if p.History60[59] != nil || p.HasCurrent {
		t.Fatal("invented a live/healthy sample for an offline node")
	}
}
