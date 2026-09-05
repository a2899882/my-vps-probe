package main

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"time"

	"my-vps-probe/common"
)

type trafficUsageView struct {
	Used            uint64 `json:"used"`
	CycleKey        string `json:"cycle_key"`
	Revision        uint64 `json:"revision"`
	UpdatedAt       int64  `json:"updated_at"`
	CalibratedAt    int64  `json:"calibrated_at"`
	CalibratedUsed  uint64 `json:"calibrated_used"`
	CalibratedCycle string `json:"calibrated_cycle"`
	PreviousUsed    uint64 `json:"previous_used"`
	BaselinePending bool   `json:"baseline_pending"`
	Note            string `json:"note"`
	Error           bool   `json:"error"`
}

func monthlyUsageSnapshot(n common.NodeConfig, now time.Time) trafficUsageView {
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()
	loadMonthlyUsageLocked()
	rec, exists := monthlyUsageState[n.ID]
	_, _, day := parseNodeQuota(n.ExpireDate)
	key := usageCycleKey(now, day)
	used := rec.Used
	if key != rec.CycleKey {
		used = 0
	}
	return trafficUsageView{Used: used, CycleKey: key, Revision: rec.Revision,
		UpdatedAt: rec.UpdatedAt, CalibratedAt: rec.CalibratedAt, CalibratedUsed: rec.CalibratedUsed,
		CalibratedCycle: rec.CalibratedCycle, PreviousUsed: rec.PreviousUsed,
		BaselinePending: !exists || rec.BaselinePending || rec.CycleKey != key,
		Note:            rec.Note, Error: monthlyUsageLoadError != nil}
}

var validTrafficAmount = regexp.MustCompile(`^(0|[1-9][0-9]{0,7})(\.[0-9]{1,6})?$`)

func parseTrafficGiB(value string) (uint64, error) {
	if !validTrafficAmount.MatchString(value) {
		return 0, errors.New("已用流量必须是非负 GB 数值，最多 6 位小数")
	}
	amount, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0, errors.New("流量数值无效")
	}
	amount.Mul(amount, new(big.Rat).SetInt64(1073741824))
	bytes := new(big.Int).Quo(amount.Num(), amount.Denom())
	if !bytes.IsUint64() || bytes.Uint64() > 9007199254740991 {
		return 0, errors.New("流量数值超出支持范围")
	}
	return bytes.Uint64(), nil
}

type trafficAdjustment struct {
	NodeID        string `json:"node_id"`
	ExpectedCycle string `json:"expected_cycle"`
	Revision      uint64 `json:"revision"`
	UsedGB        string `json:"used_gb"`
}

func trafficCalibrationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		configMutex.RLock()
		nodes := append([]common.NodeConfig(nil), appConfig.Nodes...)
		configMutex.RUnlock()
		items := make(map[string]trafficUsageView, len(nodes))
		for _, n := range nodes {
			items[n.ID] = monthlyUsageSnapshot(n, time.Now())
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"nodes": items})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "仅支持 GET、POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	var request struct {
		Adjustments []trafficAdjustment `json:"adjustments"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.Adjustments) == 0 || len(request.Adjustments) > 2000 {
		writeAPIError(w, http.StatusBadRequest, "请提供 1–2000 台节点的有效校准数据")
		return
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "请求包含多余数据")
		return
	}
	amounts := make(map[string]uint64, len(request.Adjustments))
	for _, item := range request.Adjustments {
		if _, duplicate := amounts[item.NodeID]; duplicate {
			writeAPIError(w, http.StatusBadRequest, "校准列表含重复节点")
			return
		}
		used, err := parseTrafficGiB(item.UsedGB)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		amounts[item.NodeID] = used
	}
	// Keep configuration stable through the atomic update, with the same lock
	// order used by report processing. Validate the whole batch before mutation.
	configMutex.RLock()
	defer configMutex.RUnlock()
	configured := make(map[string]common.NodeConfig, len(appConfig.Nodes))
	for _, n := range appConfig.Nodes {
		configured[n.ID] = n
	}
	now := time.Now()
	mapMutex.RLock()
	fresh := make(map[string]bool, len(amounts))
	for id := range amounts {
		fresh[id] = statusIsFresh(serverStatusMap[id], appConfig.AgentReportSeconds, now)
	}
	mapMutex.RUnlock()
	monthlyUsageMutex.Lock()
	defer monthlyUsageMutex.Unlock()
	loadMonthlyUsageLocked()
	for _, item := range request.Adjustments {
		n, exists := configured[item.NodeID]
		if !exists {
			writeAPIError(w, http.StatusNotFound, "节点不存在，请先保存节点配置")
			return
		}
		_, _, day := parseNodeQuota(n.ExpireDate)
		if item.ExpectedCycle != usageCycleKey(now, day) || item.Revision != monthlyUsageState[item.NodeID].Revision {
			writeAPIError(w, http.StatusConflict, "账期或校准记录已改变，请关闭窗口并重新打开后校准")
			return
		}
	}
	previous := make(map[string]monthlyUsageRecord, len(amounts))
	existed := make(map[string]bool, len(amounts))
	wasDirty := monthlyUsageDirty
	for _, item := range request.Adjustments {
		rec, exists := monthlyUsageState[item.NodeID]
		previous[item.NodeID], existed[item.NodeID] = rec, exists
		oldUsed := rec.Used
		if rec.CycleKey != item.ExpectedCycle {
			oldUsed = 0
		}
		rec.BaselinePending = !fresh[item.NodeID] || !rec.CountersReady || rec.CycleKey != item.ExpectedCycle
		rec.CycleKey, rec.Used = item.ExpectedCycle, amounts[item.NodeID]
		rec.Revision++
		rec.CalibratedAt, rec.CalibratedUsed, rec.CalibratedCycle = now.Unix(), rec.Used, rec.CycleKey
		rec.PreviousUsed, rec.Note = oldUsed, "已手动校准，后续按新基线继续累计"
		monthlyUsageState[item.NodeID] = rec
	}
	monthlyUsageDirty = true
	if err := saveMonthlyUsageLocked(); err != nil {
		for id, rec := range previous {
			if existed[id] {
				monthlyUsageState[id] = rec
			} else {
				delete(monthlyUsageState, id)
			}
		}
		monthlyUsageDirty = wasDirty
		writeAPIError(w, http.StatusInternalServerError, "流量状态写入失败，未进行任何校准；请检查磁盘空间及 usage_state.json")
		return
	}
	// Preserve delivered-alert state, but wait for a new sample before allowing
	// repeats or recovery decisions with the corrected quota.
	resourceMonitor.Lock()
	for id := range amounts {
		key := resourceKey(id, "traffic")
		if state, ok := resourceMonitor.states[key]; ok {
			state.Valid = false
			state.Since, state.BelowSince = time.Time{}, time.Time{}
			resourceMonitor.states[key] = state
		}
	}
	resourceMonitor.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "updated": len(amounts)})
}
