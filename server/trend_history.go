package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Aggregate only the response; the minute-level archive is never rewritten.
// An aligned bucket grid keeps gaps visible and caps work in the browser.
const trendPointLimit = 480

type trendCacheEntry struct {
	data []byte
	at   time.Time
	step int
}

var trendCache = struct {
	sync.Mutex
	entries map[string]trendCacheEntry
}{entries: make(map[string]trendCacheEntry)}

func trendStep(hours float64) int {
	return int(math.Max(1, math.Ceil(hours*60/(trendPointLimit-1)))) * 60
}

func cachedTrend(key string, now time.Time) (trendCacheEntry, bool) {
	trendCache.Lock()
	defer trendCache.Unlock()
	entry, ok := trendCache.entries[key]
	return entry, ok && now.Sub(entry.at) < 15*time.Second
}

func cacheTrend(key string, entry trendCacheEntry) {
	if len(entry.data) > 1<<20 {
		return
	}
	trendCache.Lock()
	defer trendCache.Unlock()
	total := len(entry.data)
	delete(trendCache.entries, key)
	for k, v := range trendCache.entries {
		if entry.at.Sub(v.at) >= 15*time.Second {
			delete(trendCache.entries, k)
		} else {
			total += len(v.data)
		}
	}
	for total > 4<<20 || len(trendCache.entries) >= 32 {
		oldestKey := ""
		oldest := entry.at.Add(time.Second)
		for k, v := range trendCache.entries {
			if v.at.Before(oldest) {
				oldest, oldestKey = v.at, k
			}
		}
		total -= len(trendCache.entries[oldestKey].data)
		delete(trendCache.entries, oldestKey)
	}
	trendCache.entries[key] = entry
}

func trendHistoryHandler(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeAPIError(w, http.StatusMethodNotAllowed, "仅支持 GET")
			return
		}
		id := r.URL.Query().Get("server_id")
		configMutex.RLock()
		found := false
		reportSeconds := appConfig.AgentReportSeconds
		for _, n := range appConfig.Nodes {
			if n.ID == id {
				found = true
				break
			}
		}
		tasks := pingTasksForNode(appConfig.PingTasks, id)
		configMutex.RUnlock()
		if !found {
			writeAPIError(w, http.StatusNotFound, "节点不存在")
			return
		}
		hours, err := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
		maxHours := 168.0
		if kind == "ping" {
			maxHours = 8760 // Preserve the existing extended Ping history API.
		}
		if err != nil || math.IsNaN(hours) || math.IsInf(hours, 0) || hours < .25 || hours > maxHours {
			hours = 1
			if kind == "ping" {
				hours = 24
			}
		}
		step := trendStep(hours)
		targets := make([]string, 0, len(tasks))
		for _, task := range tasks {
			targets = append(targets, task.Name)
		}
		targetKey, _ := json.Marshal(targets)
		key := fmt.Sprintf("%s|%s|%g|%s", kind, id, hours, targetKey)
		now := time.Now()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-History-Step-Seconds", strconv.Itoa(step))
		if entry, ok := cachedTrend(key, now); ok {
			_, _ = w.Write(entry.data)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		// Use SQLite's clock for both insertion and filtering. Comparing against a
		// Go-formatted wall clock made short windows sensitive to host time zones.
		window := fmt.Sprintf("-%g hours", hours)
		args := []interface{}{step, step, id, window}
		var query string
		if kind == "ping" {
			if len(targets) == 0 {
				_, _ = w.Write([]byte("[]"))
				return
			}
			placeholders := make([]string, len(targets))
			for i, target := range targets {
				placeholders[i] = "?"
				args = append(args, target)
			}
			query = `WITH buckets AS (
 SELECT CAST(strftime('%s', timestamp) AS INTEGER) / ? * ? AS bucket, *
 FROM ping_history WHERE server_id = ? AND timestamp >= datetime('now', ?) AND timestamp <= datetime('now') AND target_name IN (` + strings.Join(placeholders, ",") + `))
 SELECT datetime(bucket, 'unixepoch', 'localtime') AS time, bucket AS ts,
 target_name AS target, COALESCE(AVG(CASE WHEN delay >= 0 THEN delay END), -1) AS delay,
 AVG(COALESCE(loss_rate, CASE WHEN delay < 0 THEN 100 ELSE 0 END)) AS loss, COUNT(*) AS samples
 FROM buckets GROUP BY bucket, target_name ORDER BY bucket, target_name`
		} else {
			query = `WITH buckets AS (
 SELECT CAST(strftime('%s', timestamp) AS INTEGER) / ? * ? AS bucket, *
 FROM resource_history WHERE server_id = ? AND timestamp >= datetime('now', ?) AND timestamp <= datetime('now'))
 SELECT datetime(bucket, 'unixepoch', 'localtime') AS time, bucket AS ts, COUNT(*) AS samples,
 MAX(cpu_usage) AS cpu_usage, MAX(mem_used) AS mem_used, MAX(mem_total) AS mem_total,
 MAX(disk_used) AS disk_used, MAX(disk_total) AS disk_total,
 MAX(swap_used) AS swap_used, MAX(swap_total) AS swap_total,
 COALESCE(MAX(100.0 * mem_used / NULLIF(mem_total, 0)), 0) AS mem_usage,
 COALESCE(MAX(100.0 * disk_used / NULLIF(disk_total, 0)), 0) AS disk_usage,
 MAX(load_1) AS load_1, MAX(net_in_speed) AS net_in_speed, MAX(net_out_speed) AS net_out_speed,
 COALESCE(MAX(tcp_connections), 0) AS tcp_connections, COALESCE(MAX(udp_connections), 0) AS udp_connections
 FROM buckets GROUP BY bucket ORDER BY bucket`
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "历史查询失败，请稍后重试")
			return
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "无法读取历史字段")
			return
		}
		points := make([]map[string]interface{}, 0)
		for rows.Next() {
			values, pointers := make([]interface{}, len(columns)), make([]interface{}, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "历史记录读取失败")
				return
			}
			point := map[string]interface{}{"step_seconds": step}
			for i, column := range columns {
				if b, ok := values[i].([]byte); ok {
					point[column] = string(b)
				} else {
					point[column] = values[i]
				}
			}
			points = append(points, point)
		}
		if err := rows.Err(); err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "历史查询中断，请重试")
			return
		}
		if kind == "resource" {
			points = appendLiveResourcePoint(points, id, step, reportSeconds, now)
		}
		data, err := json.Marshal(points)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "历史数据编码失败")
			return
		}
		cacheTrend(key, trendCacheEntry{data: data, at: now, step: step})
		_, _ = w.Write(data)
	}
}

func appendLiveResourcePoint(points []map[string]interface{}, id string, step, reportSeconds int, now time.Time) []map[string]interface{} {
	// A single live value is not a trend and renders as a misleading isolated
	// dot. Wait for the first committed minute; once history exists, a fresh
	// status may still extend or merge into the continuous line below.
	if len(points) == 0 {
		return points
	}
	mapMutex.RLock()
	status, ok := serverStatusMap[id]
	mapMutex.RUnlock()
	if !ok || !statusIsFresh(status, reportSeconds, now) {
		return points
	}
	if step < 1 {
		step = 60
	}
	bucket := now.Unix() / int64(step) * int64(step)
	percent := func(used, total uint64) float64 {
		if total == 0 {
			return 0
		}
		return 100 * float64(used) / float64(total)
	}
	live := map[string]interface{}{
		"time":            time.Unix(bucket, 0).Format("2006-01-02 15:04:05"),
		"ts":              bucket,
		"samples":         int64(1),
		"step_seconds":    step,
		"live":            true,
		"cpu_usage":       status.CPUUsage,
		"mem_used":        status.MemUsed,
		"mem_total":       status.MemTotal,
		"disk_used":       status.DiskUsed,
		"disk_total":      status.DiskTotal,
		"swap_used":       status.SwapUsed,
		"swap_total":      status.SwapTotal,
		"mem_usage":       percent(status.MemUsed, status.MemTotal),
		"disk_usage":      percent(status.DiskUsed, status.DiskTotal),
		"load_1":          status.Load1,
		"net_in_speed":    status.NetInSpeed,
		"net_out_speed":   status.NetOutSpeed,
		"tcp_connections": status.TCPConnections,
		"udp_connections": status.UDPConnections,
	}
	last := points[len(points)-1]
	lastBucket, _ := numericValue(last["ts"])
	if int64(lastBucket) != bucket {
		return append(points, live)
	}
	// A bucket represents peaks. Merge the current sample without hiding a
	// spike already written earlier in the same minute/aggregate interval.
	for _, field := range []string{"cpu_usage", "mem_used", "mem_total", "disk_used", "disk_total", "swap_used", "swap_total", "mem_usage", "disk_usage", "load_1", "net_in_speed", "net_out_speed", "tcp_connections", "udp_connections"} {
		old, oldOK := numericValue(last[field])
		current, currentOK := numericValue(live[field])
		if !oldOK || currentOK && current > old {
			last[field] = live[field]
		}
	}
	last["live"] = true
	return points
}

func numericValue(value interface{}) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float64:
		return number, true
	case []byte:
		parsed, err := strconv.ParseFloat(string(number), 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(number, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
