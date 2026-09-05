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
	if !boolEnabled(tg.NotifyOffline) || !boolEnabled(tg.NotifyExpiry) || !boolEnabled(tg.NotifyRecovery) || tg.Resources.CPU.Enabled || tg.Resources.Memory.Enabled {
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
