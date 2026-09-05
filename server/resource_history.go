package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

const maxSQLiteInteger = uint64(1<<63 - 1)

// Existing installations may predate connection history. Read and close the
// schema cursor before ALTER TABLE, and never hide a failed migration: serving
// with a half-migrated table would make every trend query fail after upgrade.
func ensureResourceHistoryColumns() {
	rows, err := db.Query(`PRAGMA table_info(resource_history)`)
	if err != nil {
		log.Fatalf("inspect resource history schema: %v", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			log.Fatalf("read resource history schema: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Fatalf("read resource history schema: %v", err)
	}
	if err := rows.Close(); err != nil {
		log.Fatalf("close resource history schema cursor: %v", err)
	}
	for _, column := range []string{"tcp_connections", "udp_connections"} {
		if columns[column] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE resource_history ADD COLUMN ` + column + ` INTEGER`); err != nil {
			log.Fatalf("migrate resource history column %s: %v", column, err)
		}
	}
}

func finiteMetric(value, fallback, minValue, maxValue float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		value = fallback
		if math.IsNaN(value) || math.IsInf(value, 0) {
			value = minValue
		}
		return min(max(value, minValue), maxValue), true
	}
	clamped := min(max(value, minValue), maxValue)
	return clamped, clamped != value
}

func validPreviousUsed(previousUsed, previousTotal, currentTotal uint64) bool {
	return currentTotal > 0 && previousTotal == currentTotal && previousUsed <= currentTotal && previousUsed <= maxSQLiteInteger
}

// sanitizeReportedStatus protects both old Agents and the database. Some
// OpenVZ/LXC kernels expose MemAvailable larger than MemTotal; older gopsutil
// then underflowed Used into a value near uint64's maximum. database/sql cannot
// bind that value to SQLite, and the old all-or-nothing transaction consequently
// stopped history for every node.
func sanitizeReportedStatus(status, previous common.ServerStatus) (common.ServerStatus, []string) {
	corrected := make([]string, 0, 4)
	var changed bool
	status.CPUUsage, changed = finiteMetric(status.CPUUsage, previous.CPUUsage, 0, 100)
	if changed {
		corrected = append(corrected, "cpu_usage")
	}
	status.Load1, changed = finiteMetric(status.Load1, previous.Load1, 0, 1e9)
	if changed {
		corrected = append(corrected, "load_1")
	}

	capacities := []struct {
		name                       string
		used, total                 *uint64
		previousUsed, previousTotal uint64
	}{
		{"memory", &status.MemUsed, &status.MemTotal, previous.MemUsed, previous.MemTotal},
		{"swap", &status.SwapUsed, &status.SwapTotal, previous.SwapUsed, previous.SwapTotal},
		{"disk", &status.DiskUsed, &status.DiskTotal, previous.DiskUsed, previous.DiskTotal},
	}
	for _, capacity := range capacities {
		bad := *capacity.total > maxSQLiteInteger || *capacity.used > maxSQLiteInteger || (*capacity.total == 0 && *capacity.used > 0) || (*capacity.total > 0 && *capacity.used > *capacity.total)
		if !bad {
			continue
		}
		if *capacity.total > maxSQLiteInteger {
			*capacity.total, *capacity.used = 0, 0
		} else if validPreviousUsed(capacity.previousUsed, capacity.previousTotal, *capacity.total) {
			*capacity.used = capacity.previousUsed
		} else {
			*capacity.used = 0
		}
		corrected = append(corrected, capacity.name)
	}

	bounded := []struct {
		name            string
		value, previous *uint64
	}{
		{"net_in_speed", &status.NetInSpeed, nil}, {"net_out_speed", &status.NetOutSpeed, nil},
		{"net_in_transfer", &status.NetInTransfer, &previous.NetInTransfer},
		{"net_out_transfer", &status.NetOutTransfer, &previous.NetOutTransfer},
		{"tcp_connections", &status.TCPConnections, nil}, {"udp_connections", &status.UDPConnections, nil},
	}
	for _, field := range bounded {
		if *field.value > maxSQLiteInteger {
			*field.value = 0
			if field.previous != nil && *field.previous <= maxSQLiteInteger {
				*field.value = *field.previous
			}
			corrected = append(corrected, field.name)
		}
	}
	for i := range status.PingStatuses {
		p := &status.PingStatuses[i]
		if math.IsNaN(p.CurrentDelay) || math.IsInf(p.CurrentDelay, 0) {
			p.CurrentDelay = -1
			corrected = append(corrected, "ping")
		}
		p.AvgDelay, _ = finiteMetric(p.AvgDelay, 0, 0, 1e9)
		p.LossRate, _ = finiteMetric(p.LossRate, 100, 0, 100)
	}
	return status, corrected
}

var statusCorrectionLog = struct {
	sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time)}

func logStatusCorrections(id string, fields []string, now time.Time) {
	if len(fields) == 0 {
		return
	}
	statusCorrectionLog.Lock()
	defer statusCorrectionLog.Unlock()
	if now.Sub(statusCorrectionLog.last[id]) < time.Hour {
		return
	}
	statusCorrectionLog.last[id] = now
	log.Printf("sanitized invalid metrics from %s: %s", id, strings.Join(fields, ","))
}

type resourceHistoryWriteResult struct {
	Attempted int
	Written   int
	Err       error
}

var resourceHistoryRuntime = struct {
	sync.RWMutex
	LastAttempt int64  `json:"last_attempt"`
	LastSuccess int64  `json:"last_success"`
	Written     int    `json:"written"`
	LastError   string `json:"last_error,omitempty"`
}{}

func recordResourceHistoryResult(now time.Time, result resourceHistoryWriteResult) {
	resourceHistoryRuntime.Lock()
	defer resourceHistoryRuntime.Unlock()
	resourceHistoryRuntime.LastAttempt = now.Unix()
	resourceHistoryRuntime.Written = result.Written
	resourceHistoryRuntime.LastError = ""
	if result.Err != nil {
		resourceHistoryRuntime.LastError = result.Err.Error()
	}
	if result.Written > 0 || result.Attempted == 0 {
		resourceHistoryRuntime.LastSuccess = now.Unix()
	}
}

func resourceHistorySnapshot() map[string]interface{} {
	resourceHistoryRuntime.RLock()
	defer resourceHistoryRuntime.RUnlock()
	return map[string]interface{}{
		"last_attempt": resourceHistoryRuntime.LastAttempt,
		"last_success": resourceHistoryRuntime.LastSuccess,
		"written":      resourceHistoryRuntime.Written,
		"last_error":   resourceHistoryRuntime.LastError,
	}
}

func sqliteInteger(value uint64) int64 {
	if value > maxSQLiteInteger {
		return int64(maxSQLiteInteger)
	}
	return int64(value)
}

func saveHistoryToDBAt(now time.Time) resourceHistoryWriteResult {
	mapMutex.RLock()
	statuses := make(map[string]common.ServerStatus, len(serverStatusMap))
	for id, status := range serverStatusMap {
		statuses[id] = status
	}
	mapMutex.RUnlock()
	configMutex.RLock()
	reportSeconds := appConfig.AgentReportSeconds
	configMutex.RUnlock()

	result := resourceHistoryWriteResult{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		result.Err = fmt.Errorf("begin resource history: %w", err)
		return result
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO resource_history (
timestamp, server_id, cpu_usage, mem_used, mem_total, disk_used, disk_total,
swap_used, swap_total, load_1, net_in_speed, net_out_speed, tcp_connections, udp_connections
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		result.Err = fmt.Errorf("prepare resource history: %w", err)
		return result
	}
	defer stmt.Close()

	timestamp := now.UTC().Truncate(time.Minute).Format("2006-01-02 15:04:05")
	var writeErrors []error
	for serverID, rawStatus := range statuses {
		if !statusIsFresh(rawStatus, reportSeconds, now) {
			continue
		}
		result.Attempted++
		status, _ := sanitizeReportedStatus(rawStatus, common.ServerStatus{})
		_, err := stmt.ExecContext(ctx,
			timestamp, serverID, status.CPUUsage,
			sqliteInteger(status.MemUsed), sqliteInteger(status.MemTotal),
			sqliteInteger(status.DiskUsed), sqliteInteger(status.DiskTotal),
			sqliteInteger(status.SwapUsed), sqliteInteger(status.SwapTotal), status.Load1,
			sqliteInteger(status.NetInSpeed), sqliteInteger(status.NetOutSpeed),
			sqliteInteger(status.TCPConnections), sqliteInteger(status.UDPConnections),
		)
		if err != nil {
			writeErrors = append(writeErrors, fmt.Errorf("%s: %w", serverID, err))
			continue
		}
		result.Written++
	}
	if err := tx.Commit(); err != nil {
		result.Written = 0
		result.Err = fmt.Errorf("commit resource history: %w", err)
		return result
	}
	result.Err = errors.Join(writeErrors...)
	return result
}

func saveHistoryToDB() {
	now := time.Now()
	result := saveHistoryToDBAt(now)
	recordResourceHistoryResult(now, result)
	if result.Err != nil {
		log.Printf("resource history wrote %d/%d nodes: %v", result.Written, result.Attempted, result.Err)
	}
}
