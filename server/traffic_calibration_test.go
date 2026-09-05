package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"my-vps-probe/common"
)

func trafficTestState(t *testing.T) {
	t.Helper()
	oldState, oldLoaded, oldDirty := monthlyUsageState, monthlyUsageLoaded, monthlyUsageDirty
	oldPath, oldError := monthlyUsagePath, monthlyUsageLoadError
	oldConfig, oldStatuses := appConfig, serverStatusMap
	monthlyUsageState, monthlyUsageLoaded, monthlyUsageDirty = make(map[string]monthlyUsageRecord), true, false
	monthlyUsagePath, monthlyUsageLoadError = filepath.Join(t.TempDir(), "usage_state.json"), nil
	appConfig, serverStatusMap = validTestConfig(), make(map[string]common.ServerStatus)
	appConfig.Nodes = append(appConfig.Nodes, common.NodeConfig{ID: "new", Name: "New", Token: "new-token"})
	t.Cleanup(func() {
		monthlyUsageState, monthlyUsageLoaded, monthlyUsageDirty = oldState, oldLoaded, oldDirty
		monthlyUsagePath, monthlyUsageLoadError = oldPath, oldError
		appConfig, serverStatusMap = oldConfig, oldStatuses
	})
}

func TestTrafficIndependentCountersRebootAndCollectionFailure(t *testing.T) {
	trafficTestState(t)
	now := time.Now()
	sample := common.ServerStatus{NetInTransfer: 1000, NetOutTransfer: 2000, Uptime: 500, BootID: "boot-a"}
	observeMonthlyUsage("node-1", "", sample, now)
	sample.NetInTransfer, sample.NetOutTransfer, sample.Uptime = 1100, 2200, 510
	observeMonthlyUsage("node-1", "", sample, now.Add(time.Second))
	sample.NetInTransfer, sample.NetOutTransfer = 5, 2250
	observeMonthlyUsage("node-1", "", sample, now.Add(2*time.Second))
	if got := getMonthlyUsage("node-1"); got != 350 {
		t.Fatalf("one counter reset cancelled the other direction: %d", got)
	}
	valid := false
	sample.NetworkValid = &valid
	sample.NetInTransfer, sample.NetOutTransfer = 0, 0
	observeMonthlyUsage("node-1", "", sample, now.Add(3*time.Second))
	if monthlyUsageState["node-1"].LastOut != 2250 {
		t.Fatal("failed collection replaced the valid baseline")
	}
	valid = true
	sample.BootID, sample.Uptime = "boot-b", 520 // Even a higher uptime/counter is a new boot.
	sample.NetInTransfer, sample.NetOutTransfer = 2000, 3000
	observeMonthlyUsage("node-1", "", sample, now.Add(4*time.Second))
	if got := getMonthlyUsage("node-1"); got != 5350 {
		t.Fatalf("first counters after known reboot were lost: %d", got)
	}
}

func TestTrafficInterfacesDoNotCancelEachOtherOrImportOldCounters(t *testing.T) {
	trafficTestState(t)
	now := time.Now()
	for i, counters := range [][]common.NetCounter{
		{{Name: "eth0", In: 1000}, {Name: "lo", In: 500}},
		{{Name: "eth0", In: 1100}, {Name: "lo", In: 550}},
		{{Name: "eth0", In: 1200}},
		{{Name: "eth0", In: 1300}, {Name: "lo", In: 8000}},
	} {
		sample := common.ServerStatus{NetCounters: counters}
		for _, c := range counters {
			sample.NetInTransfer += c.In
		}
		observeMonthlyUsage("node-1", "", sample, now.Add(time.Duration(i)*time.Second))
	}
	if got := getMonthlyUsage("node-1"); got != 350 {
		t.Fatalf("interface replacement imported historic traffic: %d", got)
	}
}

func adjustTraffic(t *testing.T, adjustments []trafficAdjustment, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{"adjustments": adjustments})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/admin/traffic", bytes.NewReader(data))
	if authorized {
		r.SetBasicAuth(appConfig.AdminUser, appConfig.AdminPass)
	}
	w := httptest.NewRecorder()
	basicAuth(trafficCalibrationHandler)(w, r)
	return w
}

func TestTrafficOnlineCalibrationPersistsAndContinuesOnce(t *testing.T) {
	trafficTestState(t)
	now := time.Now()
	sample := common.ServerStatus{IsOnline: true, LastReport: now.Unix(), NetInTransfer: 1000, NetOutTransfer: 2000}
	observeMonthlyUsage("node-1", "", sample, now)
	serverStatusMap["node-1"] = sample
	adjustment := trafficAdjustment{NodeID: "node-1", ExpectedCycle: usageCycleKey(now, 1), UsedGB: "1.25"}
	if w := adjustTraffic(t, []trafficAdjustment{adjustment}, false); w.Code != 401 {
		t.Fatalf("unauthenticated calibration: %d", w.Code)
	}
	if w := adjustTraffic(t, []trafficAdjustment{adjustment}, true); w.Code != 200 {
		t.Fatalf("calibration: %d %s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(monthlyUsagePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]monthlyUsageRecord
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["node-1"].Used != 1342177280 || saved["node-1"].Revision != 1 {
		t.Fatalf("not persisted: %+v", saved)
	}
	if w := adjustTraffic(t, []trafficAdjustment{adjustment}, true); w.Code != 409 {
		t.Fatalf("stale calibration should conflict: %d", w.Code)
	}
	sample.NetInTransfer += 100
	sample.NetOutTransfer += 200
	observeMonthlyUsage("node-1", "", sample, now.Add(time.Second))
	observeMonthlyUsage("node-1", "", sample, now.Add(2*time.Second))
	if got := getMonthlyUsage("node-1"); got != 1342177580 {
		t.Fatalf("calibrated increment counted incorrectly: %d", got)
	}
}

func TestTrafficOfflineAndNewNodeCalibrationSurvivesRestart(t *testing.T) {
	trafficTestState(t)
	now := time.Now()
	updateMonthlyUsage("node-1", "", 1000, 2000)
	items := []trafficAdjustment{{NodeID: "node-1", ExpectedCycle: usageCycleKey(now, 1), UsedGB: "0"}, {NodeID: "new", ExpectedCycle: usageCycleKey(now, 1), UsedGB: "2"}}
	if w := adjustTraffic(t, items, true); w.Code != 200 {
		t.Fatalf("batch: %s", w.Body.String())
	}
	monthlyUsageLoaded, monthlyUsageState = false, nil
	for _, id := range []string{"node-1", "new"} {
		observeMonthlyUsage(id, "", common.ServerStatus{NetInTransfer: 500000, NetOutTransfer: 600000}, now.Add(time.Second))
		observeMonthlyUsage(id, "", common.ServerStatus{NetInTransfer: 500100, NetOutTransfer: 600050}, now.Add(2*time.Second))
	}
	if getMonthlyUsage("node-1") != 150 || getMonthlyUsage("new") != 2147483798 {
		t.Fatalf("offline/new baseline imported old counters: %+v", monthlyUsageState)
	}
}

func TestTrafficBatchValidationAndDiskFailureAreAtomic(t *testing.T) {
	trafficTestState(t)
	now := time.Now()
	updateMonthlyUsage("node-1", "", 100, 200)
	first := trafficAdjustment{NodeID: "node-1", ExpectedCycle: usageCycleKey(now, 1), UsedGB: "2"}
	unknown := first
	unknown.NodeID = "unknown"
	if w := adjustTraffic(t, []trafficAdjustment{first, unknown}, true); w.Code != 404 {
		t.Fatalf("invalid batch: %d", w.Code)
	}
	if getMonthlyUsage("node-1") != 0 {
		t.Fatal("partially applied invalid batch")
	}
	monthlyUsagePath = t.TempDir() // Rename to a directory must fail, including as root.
	if w := adjustTraffic(t, []trafficAdjustment{first}, true); w.Code != 500 {
		t.Fatalf("disk failure: %d", w.Code)
	}
	if monthlyUsageState["node-1"].Revision != 0 || getMonthlyUsage("node-1") != 0 || !monthlyUsageDirty {
		t.Fatal("failed write was not rolled back")
	}
}

func TestTrafficCycleRolloverAndDecimalValidation(t *testing.T) {
	trafficTestState(t)
	zone := time.FixedZone("UTC+8", 8*3600)
	before := time.Date(2026, 9, 4, 23, 59, 59, 0, zone)
	after := before.Add(2 * time.Second)
	observeMonthlyUsage("node-1", "2030/01/01|100|5", common.ServerStatus{NetInTransfer: 100}, before)
	observeMonthlyUsage("node-1", "2030/01/01|100|5", common.ServerStatus{NetInTransfer: 200}, before)
	if getMonthlyUsageForCycle("node-1", "2030/01/01|100|5", after) != 0 {
		t.Fatal("offline node did not roll over")
	}
	observeMonthlyUsage("node-1", "2030/01/01|100|5", common.ServerStatus{NetInTransfer: 300}, after)
	if getMonthlyUsage("node-1") != 0 {
		t.Fatal("previous cycle imported into new cycle")
	}
	for _, value := range []string{"-1", "NaN", "1e5", "1.1234567", "99999999", "", "00"} {
		if _, err := parseTrafficGiB(value); err == nil {
			t.Fatalf("invalid amount accepted: %q", value)
		}
	}
	if got, err := parseTrafficGiB("1.000001"); err != nil || got != 1073742897 {
		t.Fatalf("decimal conversion: %d %v", got, err)
	}
}

func TestCorruptTrafficStateCannotBeOverwritten(t *testing.T) {
	trafficTestState(t)
	if err := os.WriteFile(monthlyUsagePath, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	monthlyUsageLoaded = false
	updateMonthlyUsage("node-1", "", 100, 200)
	flushMonthlyUsage()
	data, _ := os.ReadFile(monthlyUsagePath)
	if string(data) != "not-json" || !monthlyUsageSnapshot(appConfig.Nodes[0], time.Now()).Error {
		t.Fatal("corrupt accounting state silently overwritten")
	}
}
