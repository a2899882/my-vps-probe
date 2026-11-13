package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"my-vps-probe/common"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func testTelegramConfig() TelegramConfig {
	t := TelegramConfig{Enabled: true, Token: "synthetic-test-token", ChatID: "synthetic-chat"}
	t.normalize()
	return t
}
func TestTelegramOldConfigDefaultsAndIndependentSwitches(t *testing.T) {
	var tg TelegramConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"offline_minutes":5,"expiry_reminder_days":7}`), &tg); err != nil {
		t.Fatal(err)
	}
	tg.normalize()
	if !boolEnabled(tg.NotifyOffline) || !boolEnabled(tg.NotifyExpiry) || !boolEnabled(tg.NotifyRecovery) || tg.Resources.CPU.Enabled || tg.Resources.Memory.Enabled || tg.TCPBlock.Enabled {
		t.Fatal("old configuration defaults changed")
	}
	if tg.OfflineMinutes != 5 || tg.ExpiryReminderDays != 7 {
		t.Fatal("old reminder settings lost")
	}
	off := false
	tg.NotifyOffline = &off
	tg.Resources.CPU = ResourceAlertRule{Enabled: true, Threshold: 95, DurationSeconds: 0}
	tg.normalize()
	if boolEnabled(tg.NotifyOffline) || tg.Resources.CPU.DurationSeconds != 0 || tg.Resources.CPU.Threshold != 95 {
		t.Fatal("explicit false or zero setting was overwritten")
	}
}

func TestCPUAlertRequiresContinuousLoadAndConfirmedRecovery(t *testing.T) {
	now := time.Date(2026, 9, 5, 5, 0, 0, 0, time.UTC)
	tg := testTelegramConfig()
	tg.Resources.CPU = ResourceAlertRule{true, 90, 60}
	tg.RecoverySeconds = 30
	m := resourceAlertMonitor{states: make(map[string]resourceAlertState)}
	n := common.NodeConfig{ID: "n", Name: "test"}
	key := resourceKey("n", "cpu")
	observe := func(seconds int, value float64) resourceAlertState {
		ts := now.Add(time.Duration(seconds) * time.Second)
		m.observe(n, common.ServerStatus{CPUCores: 2, CPUUsage: value, IsOnline: true, LastReport: ts.Unix()}, tg, 3, ts)
		return m.snapshot()[key]
	}
	observe(0, 95)
	observe(30, 80)
	observe(50, 95)
	if observe(105, 95).Firing {
		t.Fatal("short spikes or interrupted load should not fire")
	}
	if !observe(110, 95).Firing {
		t.Fatal("continuous 60s high load should fire")
	}
	if !observe(115, 88).Firing {
		t.Fatal("hysteresis band must not immediately recover")
	}
	observe(120, 80)
	if !observe(149, 80).Firing {
		t.Fatal("recovery confirmation was shortened")
	}
	if observe(150, 80).Firing {
		t.Fatal("confirmed recovery should clear condition")
	}
	observe(151, 95)
	if observe(280, 95).Firing {
		t.Fatal("a gap in reports must reset pending duration")
	}
}

func TestMissingResourceMetricsDoNotTriggerRecovery(t *testing.T) {
	tg := testTelegramConfig()
	tg.Resources.Memory = ResourceAlertRule{true, 90, 0}
	m := resourceAlertMonitor{states: make(map[string]resourceAlertState)}
	n := common.NodeConfig{ID: "n"}
	now := time.Now()
	m.observe(n, common.ServerStatus{MemUsed: 95, MemTotal: 100}, tg, 3, now)
	m.observe(n, common.ServerStatus{}, tg, 3, now.Add(time.Second))
	state := m.snapshot()[resourceKey("n", "memory")]
	if !state.Firing || state.Valid {
		t.Fatal("missing measurements became a healthy reading")
	}
}

func TestNotificationRestartOfflineRecoveryAndExclusion(t *testing.T) {
	tg := testTelegramConfig()
	now := time.Now()
	n := common.NodeConfig{ID: "n", Name: "test"}
	events := map[string]time.Time{"offline:n": now.Add(-time.Hour)}
	messages, clear := collectNotifications(tg, []common.NodeConfig{n}, map[string]common.ServerStatus{}, nil, events, 3, now)
	if len(messages) != 0 || len(clear) != 0 {
		t.Fatal("a server restart falsely recovered an offline event")
	}
	statuses := map[string]common.ServerStatus{"n": {IsOnline: true, LastReport: now.Unix()}}
	messages, _ = collectNotifications(tg, []common.NodeConfig{n}, statuses, nil, events, 3, now)
	if len(messages) != 1 || !messages[0].Recovery {
		t.Fatal("fresh heartbeat did not recover offline event")
	}
	tg.ExcludedNodeIDs = []string{"n"}
	messages, clear = collectNotifications(tg, []common.NodeConfig{n}, statuses, nil, events, 3, now)
	if len(messages) != 0 || len(clear) != 1 {
		t.Fatal("excluded node still emits notifications")
	}
}

func TestResourceDedupRepeatAndDeliveryAcknowledgement(t *testing.T) {
	database := notificationTestDB(t)
	tg := testTelegramConfig()
	tg.Resources.Disk = ResourceAlertRule{true, 90, 0}
	now := time.Now().Truncate(time.Second)
	n := common.NodeConfig{ID: "n", Name: "test"}
	key := resourceKey("n", "disk")
	states := map[string]resourceAlertState{key: {Valid: true, Firing: true, LastReport: now, Since: now, Value: 95}}
	statuses := map[string]common.ServerStatus{"n": {IsOnline: true, LastReport: now.Unix()}}
	messages, _ := collectNotifications(tg, []common.NodeConfig{n}, statuses, states, map[string]time.Time{}, 3, now)
	if len(messages) != 1 {
		t.Fatal("initial alarm missing")
	}
	recordNotificationDelivery(messages, errors.New("synthetic transport failure"), now)
	events, _ := loadNotificationEvents()
	if len(events) != 0 {
		t.Fatal("failed send was acknowledged")
	}
	recordNotificationDelivery(messages, nil, now)
	events, _ = loadNotificationEvents()
	messages, _ = collectNotifications(tg, []common.NodeConfig{n}, statuses, states, events, 3, now.Add(time.Second))
	if len(messages) != 0 {
		t.Fatal("duplicate alarm")
	}
	tg.RepeatMinutes = 1
	messages, _ = collectNotifications(tg, []common.NodeConfig{n}, statuses, states, events, 3, now.Add(time.Minute))
	if len(messages) != 1 {
		t.Fatal("repeat interval ignored")
	}
	states[key] = resourceAlertState{Valid: true, Firing: false, LastReport: now}
	messages, _ = collectNotifications(tg, []common.NodeConfig{n}, statuses, states, events, 3, now.Add(time.Second))
	if len(messages) != 1 || !messages[0].Recovery {
		t.Fatal("resource recovery missing")
	}
	recordNotificationDelivery(messages, nil, now)
	events, _ = loadNotificationEvents()
	if len(events) != 0 {
		t.Fatal("recovery did not re-arm")
	}
	var count int
	database.QueryRow(`SELECT count(*) FROM notification_log`).Scan(&count)
	if count != 3 {
		t.Fatal("delivery log incomplete")
	}
}

func TestTCPBlockAlertDurationMaxCountAndNodeSelection(t *testing.T) {
	notificationTestDB(t)
	tcpBlockMonitor.Lock()
	tcpBlockMonitor.states = make(map[string]tcpBlockAlertState)
	tcpBlockMonitor.Unlock()
	t.Cleanup(func() {
		tcpBlockMonitor.Lock()
		tcpBlockMonitor.states = make(map[string]tcpBlockAlertState)
		tcpBlockMonitor.Unlock()
	})

	now := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	tg := testTelegramConfig()
	tg.RecoverySeconds = 30
	tg.TCPBlock = TCPBlockAlertConfig{
		Enabled:         true,
		DurationSeconds: 120,
		MaxNotifyCount:  2,
		RepeatMinutes:   3,
		NodeIDs:         []string{"n1"},
	}
	tg.normalize()

	n1 := common.NodeConfig{ID: "n1", Name: "US-LA-01", Group: "US优化"}
	n2 := common.NodeConfig{ID: "n2", Name: "HK-01", Group: "HK"}
	tasks := []common.PingTask{
		{Name: "广东电信", Host: "ct:80"},
		{Name: "广东联通", Host: "cu:80"},
		{Name: "广东移动", Host: "cm:80"},
	}
	allDown := []common.PingResult{
		{TargetName: "广东电信", CurrentDelay: -1},
		{TargetName: "广东联通", CurrentDelay: -1},
		{TargetName: "广东移动", CurrentDelay: -1},
	}
	oneUp := []common.PingResult{
		{TargetName: "广东电信", CurrentDelay: -1},
		{TargetName: "广东联通", CurrentDelay: 165},
		{TargetName: "广东移动", CurrentDelay: -1},
	}

	// Partial failure should not trigger TCP block
	tcpBlockMonitor.observe(n1, tasks, common.ServerStatus{IsOnline: true, LastReport: now.Unix(), PingStatuses: oneUp}, tg, 3, now)
	if tcpBlockMonitor.snapshot()[tcpBlockKey("n1")].Firing {
		t.Fatal("single carrier up should not fire TCP block")
	}

	// All 3 carriers fail at t=0, t=60 (not yet 120s), t=120 (fires!)
	tcpBlockMonitor.observe(n1, tasks, common.ServerStatus{IsOnline: true, LastReport: now.Unix(), PingStatuses: allDown}, tg, 3, now)
	tcpBlockMonitor.observe(n2, tasks, common.ServerStatus{IsOnline: true, LastReport: now.Unix(), PingStatuses: allDown}, tg, 3, now)
	t60 := now.Add(60 * time.Second)
	tcpBlockMonitor.observe(n1, tasks, common.ServerStatus{IsOnline: true, LastReport: t60.Unix(), PingStatuses: allDown}, tg, 3, t60)
	if tcpBlockMonitor.snapshot()[tcpBlockKey("n1")].Firing {
		t.Fatal("TCP block fired before duration_seconds elapsed")
	}
	t120 := now.Add(120 * time.Second)
	stMap := map[string]common.ServerStatus{
		"n1": {IsOnline: true, LastReport: t120.Unix(), PingStatuses: allDown},
		"n2": {IsOnline: true, LastReport: t120.Unix(), PingStatuses: allDown},
	}
	tcpBlockMonitor.observe(n1, tasks, stMap["n1"], tg, 3, t120)
	tcpBlockMonitor.observe(n2, tasks, stMap["n2"], tg, 3, t120)

	// Only n1 is in NodeIDs, so only n1 should generate a notification (1/2)
	events, _ := loadNotificationEvents()
	msgs, _ := collectNotifications(tg, []common.NodeConfig{n1, n2}, stMap, nil, events, 3, t120)
	if len(msgs) != 1 || msgs[0].NodeID != "n1" || msgs[0].Kind != "tcp_block" || !strings.Contains(msgs[0].Message, "第 1 / 2 次") {
		t.Fatalf("unexpected 1st TCP block notification: %+v", msgs)
	}
	recordNotificationDelivery(msgs, nil, t120)

	// After 3 minutes, 2nd notification (2/2) should fire
	t300 := t120.Add(3 * time.Minute)
	stMap["n1"] = common.ServerStatus{IsOnline: true, LastReport: t300.Unix(), PingStatuses: allDown}
	tcpBlockMonitor.observe(n1, tasks, stMap["n1"], tg, 3, t300)
	events, _ = loadNotificationEvents()
	msgs, _ = collectNotifications(tg, []common.NodeConfig{n1, n2}, stMap, nil, events, 3, t300)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Message, "第 2 / 2 次") {
		t.Fatalf("unexpected 2nd TCP block notification: %+v", msgs)
	}
	recordNotificationDelivery(msgs, nil, t300)

	// After another 3 minutes, MaxNotifyCount (2) is reached -> must NOT send a 3rd alert
	t480 := t300.Add(3 * time.Minute)
	stMap["n1"] = common.ServerStatus{IsOnline: true, LastReport: t480.Unix(), PingStatuses: allDown}
	tcpBlockMonitor.observe(n1, tasks, stMap["n1"], tg, 3, t480)
	events, _ = loadNotificationEvents()
	msgs, _ = collectNotifications(tg, []common.NodeConfig{n1, n2}, stMap, nil, events, 3, t480)
	if len(msgs) != 0 {
		t.Fatalf("exceeded MaxNotifyCount=2: %+v", msgs)
	}

	// Recovery after 30s
	t500 := t480.Add(10 * time.Second)
	tcpBlockMonitor.observe(n1, tasks, common.ServerStatus{IsOnline: true, LastReport: t500.Unix(), PingStatuses: oneUp}, tg, 3, t500)
	t535 := t500.Add(35 * time.Second)
	stMap["n1"] = common.ServerStatus{IsOnline: true, LastReport: t535.Unix(), PingStatuses: oneUp}
	tcpBlockMonitor.observe(n1, tasks, stMap["n1"], tg, 3, t535)
	events, _ = loadNotificationEvents()
	msgs, _ = collectNotifications(tg, []common.NodeConfig{n1, n2}, stMap, nil, events, 3, t535)
	if len(msgs) != 1 || !msgs[0].Recovery || msgs[0].Kind != "tcp_block_recovery" {
		t.Fatalf("expected TCP block recovery notification, got: %+v", msgs)
	}
	recordNotificationDelivery(msgs, nil, t535)
}

func TestExpiryIncludesTodayAndTrafficCycle(t *testing.T) {
	now := time.Now()
	tg := testTelegramConfig()
	n := common.NodeConfig{ID: "n", Name: "test", ExpireDate: now.Format("2006/01/02") + "|100|1"}
	messages, _ := collectNotifications(tg, []common.NodeConfig{n}, nil, nil, nil, 3, now)
	if len(messages) != 1 || messages[0].Kind != "expiry" {
		t.Fatal("expiry day reminder missing")
	}
	monthlyUsageMutex.Lock()
	oldState, oldLoaded := monthlyUsageState, monthlyUsageLoaded
	monthlyUsageLoaded = true
	monthlyUsageState = map[string]monthlyUsageRecord{"n": {CycleKey: usageCycleKey(now, 1), Used: 85 * 1073741824}}
	monthlyUsageMutex.Unlock()
	t.Cleanup(func() {
		monthlyUsageMutex.Lock()
		monthlyUsageState, monthlyUsageLoaded = oldState, oldLoaded
		monthlyUsageMutex.Unlock()
	})
	value, _, ok := resourceValue("traffic", n, common.ServerStatus{}, now)
	if !ok || value != 85 {
		t.Fatalf("traffic metric=%v/%v", value, ok)
	}
	if getMonthlyUsageForCycle("n", n.ExpireDate, now.AddDate(0, 1, 0)) != 0 {
		t.Fatal("offline node retained previous-cycle quota")
	}
	n.ExpireDate = "|0|1"
	_, _, ok = resourceValue("traffic", n, common.ServerStatus{}, now)
	if ok {
		t.Fatal("unlimited node triggered quota alert")
	}
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramLimitsAndSecretRedaction(t *testing.T) {
	old := telegramClient
	t.Cleanup(func() { telegramClient = old })
	tg := testTelegramConfig()
	telegramClient = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"parameters":{"retry_after":75}}`)), Header: make(http.Header)}, nil
	})}
	err := sendTelegram(tg, "test")
	var retry *telegramRetryError
	if !errors.As(err, &retry) || retry.Seconds != 75 {
		t.Fatalf("rate limit not handled: %v", err)
	}
	telegramClient = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("failed at %s", r.URL) })}
	err = sendTelegram(tg, "test")
	if err == nil || strings.Contains(err.Error(), tg.Token) {
		t.Fatal("Bot Token leaked in error")
	}
	// Telegram returns the sent Message, including its full Unicode text. A 4KB
	// response limit incorrectly treats a successful merged message as a failure.
	telegramClient = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]interface{}{"ok": true, "result": map[string]string{"text": strings.Repeat("告警", 3000)}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	if err := sendTelegram(tg, "synthetic long batch"); err != nil {
		t.Fatalf("large successful response rejected: %v", err)
	}
	pending := make([]notification, 20)
	for i := range pending {
		pending[i].Message = strings.Repeat("📡", 200)
	}
	batch, text := notificationBatch("test", pending)
	if len(batch) == 0 || len(batch) >= 20 || len(utf16.Encode([]rune(text))) > 3800 {
		t.Fatal("Telegram message limit exceeded")
	}
}
