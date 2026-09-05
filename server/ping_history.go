package main

import (
	"context"
	"database/sql"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type pingMinuteKey struct {
	Server, Target string
	Minute         int64
}

type pingMinute struct {
	Sum            float64
	Success, Count int
}

func (p pingMinute) values() (float64, float64) {
	delay := -1.0
	if p.Success > 0 {
		delay = p.Sum / float64(p.Success)
	}
	loss := 0.0
	if p.Count > 0 {
		loss = 100 * float64(p.Count-p.Success) / float64(p.Count)
	}
	return delay, loss
}

// Keep one real probe result per target and UTC minute until that minute is
// closed and committed. Agents can report status more frequently than Ping is
// displayed; retaining the first valid result gives every card cell one clear
// one-minute meaning without manufacturing averages from repeated reports.
// A delayed database write must not move a sample into a neighbouring minute.
var pingFlushMu sync.Mutex

var pingBuffer = struct {
	sync.RWMutex
	items map[pingMinuteKey]pingMinute
}{items: make(map[pingMinuteKey]pingMinute)}

func recordPingSamples(id string, pings []common.PingResult, now time.Time) {
	pingBuffer.Lock()
	defer pingBuffer.Unlock()
	minute := now.Unix() / 60
	for _, p := range pings {
		if math.IsNaN(p.CurrentDelay) || math.IsInf(p.CurrentDelay, 0) {
			continue
		}
		key := pingMinuteKey{id, p.TargetName, minute}
		if _, recorded := pingBuffer.items[key]; recorded {
			continue
		}
		v := pingMinute{Count: 1}
		if p.CurrentDelay >= 0 {
			v.Success = 1
			v.Sum = p.CurrentDelay
		}
		pingBuffer.items[key] = v
	}
}

func pendingPingMinutes() map[pingMinuteKey]pingMinute {
	pingBuffer.RLock()
	defer pingBuffer.RUnlock()
	out := make(map[pingMinuteKey]pingMinute, len(pingBuffer.items))
	for k, v := range pingBuffer.items {
		out[k] = v
	}
	return out
}

func flushPingMinutes(now time.Time, includeCurrent bool) {
	pingFlushMu.Lock()
	defer pingFlushMu.Unlock()
	// Keep at most one hour during a database outage; every discarded gap remains
	// visibly missing instead of being replaced by invented successful samples.
	pingBuffer.Lock()
	for k := range pingBuffer.items {
		if k.Minute < now.Unix()/60-60 {
			delete(pingBuffer.items, k)
		}
	}
	pingBuffer.Unlock()
	snapshot := pendingPingMinutes()
	current := now.Unix() / 60
	for k := range snapshot {
		if k.Minute >= current && !includeCurrent {
			delete(snapshot, k)
		}
	}
	if len(snapshot) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		log.Printf("begin ping history: %v", err)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO ping_history(timestamp,server_id,target_name,delay,loss_rate) VALUES(?,?,?,?,?)`)
	if err != nil {
		log.Printf("prepare ping history: %v", err)
		return
	}
	defer stmt.Close()
	for k, v := range snapshot {
		delay, loss := v.values()
		if _, err := stmt.ExecContext(ctx, time.Unix(k.Minute*60, 0).UTC().Format("2006-01-02 15:04:05"), k.Server, k.Target, delay, loss); err != nil {
			log.Printf("write ping history: %v", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit ping history: %v", err)
		return
	}
	// Invalidate before removal: requests already using the old cache still see
	// the buffer; subsequent requests read the committed rows.
	invalidateCardPingCache()
	pingBuffer.Lock()
	for k, v := range snapshot {
		if pingBuffer.items[k] == v {
			delete(pingBuffer.items, k)
		}
	}
	pingBuffer.Unlock()
}

type pingHistoryPoint struct {
	Minute      int64
	Target      string
	Delay, Loss float64
}
type cachedPingHistory struct {
	Minute   int64
	ByServer map[string][]pingHistoryPoint
	Loaded   map[string]bool
}

var pingHistoryCache = struct {
	sync.Mutex
	entry cachedPingHistory
}{}

func invalidateCardPingCache() {
	pingHistoryCache.Lock()
	pingHistoryCache.entry = cachedPingHistory{}
	pingHistoryCache.Unlock()
}

func readCardPingHistory(database *sql.DB, id string, now time.Time) ([]pingHistoryPoint, error) {
	start := time.Unix((now.Unix()/60-59)*60, 0).UTC().Format("2006-01-02 15:04:05")
	rows, err := database.Query(`SELECT unixepoch(timestamp)/60,target_name,delay,loss_rate FROM ping_history WHERE server_id=? AND timestamp>=? ORDER BY timestamp,id`, id, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []pingHistoryPoint
	for rows.Next() {
		var p pingHistoryPoint
		if err := rows.Scan(&p.Minute, &p.Target, &p.Delay, &p.Loss); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// Load the complete recent card window in one indexed query. The previous
// per-node cache still performed up to N SQLite queries after every minute
// flush; with 56 nodes that made /api/status take 7-9 seconds and caused the
// apparent 5-6 second (or worse) refresh lag in the browser.
func readAllCardPingHistory(database *sql.DB, ids []string, now time.Time) (map[string][]pingHistoryPoint, error) {
	if len(ids) == 0 {
		return make(map[string][]pingHistoryPoint), nil
	}
	start := time.Unix((now.Unix()/60-59)*60, 0).UTC().Format("2006-01-02 15:04:05")
	placeholders := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)+1)
	args = append(args, start)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	query := `SELECT unixepoch(timestamp)/60,server_id,target_name,delay,loss_rate FROM ping_history WHERE timestamp>=? AND server_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY server_id,timestamp,id`
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byServer := make(map[string][]pingHistoryPoint)
	for rows.Next() {
		var server string
		var point pingHistoryPoint
		if err := rows.Scan(&point.Minute, &server, &point.Target, &point.Delay, &point.Loss); err != nil {
			return nil, err
		}
		byServer[server] = append(byServer[server], point)
	}
	return byServer, rows.Err()
}

func primeCardPingHistory(ids []string, now time.Time) {
	pingHistoryCache.Lock()
	defer pingHistoryCache.Unlock()
	entry := pingHistoryCache.entry
	minute := now.Unix() / 60
	allLoaded := entry.Minute == minute && entry.ByServer != nil && entry.Loaded != nil
	if allLoaded {
		for _, id := range ids {
			if !entry.Loaded[id] {
				allLoaded = false
				break
			}
		}
	}
	if allLoaded {
		return
	}
	requested := make([]string, 0, len(ids)+len(entry.Loaded))
	seen := make(map[string]bool, len(ids)+len(entry.Loaded))
	if entry.Minute == minute {
		for id := range entry.Loaded {
			if !seen[id] {
				seen[id] = true
				requested = append(requested, id)
			}
		}
	}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			requested = append(requested, id)
		}
	}
	byServer, err := readAllCardPingHistory(db, requested, now)
	if err != nil {
		log.Printf("read card ping history: %v", err)
		// Cache an empty database part for this minute too. Retrying the same
		// busy query once per node would amplify a transient DB problem N times;
		// pending in-memory minutes and current values remain available.
		byServer = make(map[string][]pingHistoryPoint)
	}
	pingHistoryCache.entry = cachedPingHistory{Minute: minute, ByServer: byServer, Loaded: seen}
}

func cardPingStatuses(id string, tasks []common.PingTask, st common.ServerStatus, reportSeconds int, now time.Time) []common.CardPingStatus {
	// Serialize the cache swap with buffer removal so a committed minute never
	// disappears between the database snapshot and the in-memory pending set.
	minute := now.Unix() / 60
	var entry cachedPingHistory
	for {
		primeCardPingHistory([]string{id}, now)
		pingHistoryCache.Lock()
		entry = pingHistoryCache.entry
		if entry.Minute == minute && entry.ByServer != nil && entry.Loaded[id] {
			break
		}
		// A minute flush may invalidate the cache after primeCardPingHistory
		// returns. Retry before taking the pending-buffer snapshot so that the
		// just-committed minute cannot disappear from both sources.
		pingHistoryCache.Unlock()
	}
	pingBuffer.RLock()
	buffer := make(map[pingMinuteKey]pingMinute)
	for k, v := range pingBuffer.items {
		if k.Server == id {
			buffer[k] = v
		}
	}
	pingBuffer.RUnlock()
	pingHistoryCache.Unlock()
	return buildCardPingStatuses(id, tasks, entry.ByServer[id], buffer, st, reportSeconds, now)
}

func buildCardPingStatuses(id string, tasks []common.PingTask, points []pingHistoryPoint, pending map[pingMinuteKey]pingMinute, st common.ServerStatus, reportSeconds int, now time.Time) []common.CardPingStatus {
	buckets := make(map[pingMinuteKey]pingHistoryPoint, len(points)+len(pending))
	for _, p := range points {
		buckets[pingMinuteKey{id, p.Target, p.Minute}] = p
	}
	for k, v := range pending {
		if k.Server != id {
			continue
		}
		delay, loss := v.values()
		buckets[k] = pingHistoryPoint{k.Minute, k.Target, delay, loss}
	}
	start := now.Unix()/60 - 59
	current := make(map[string]float64)
	if statusIsFresh(st, reportSeconds, now) {
		for _, p := range st.PingStatuses {
			current[p.TargetName] = p.CurrentDelay
		}
	}
	out := make([]common.CardPingStatus, 0, len(tasks))
	for _, task := range tasks {
		p := common.CardPingStatus{TargetName: task.Name, History60: make([]*float64, 60), HistoryLoss60: make([]*float64, 60), HistoryStart: start * 60}
		valid := 0
		sum, lossSum := 0.0, 0.0
		for i := 0; i < 60; i++ {
			value, ok := buckets[pingMinuteKey{id, task.Name, start + int64(i)}]
			if !ok {
				continue
			}
			delay, loss := value.Delay, value.Loss
			p.History60[i] = &delay
			p.HistoryLoss60[i] = &loss
			p.SampleMinutes++
			lossSum += loss
			if delay >= 0 {
				valid++
				sum += delay
			}
		}
		if valid > 0 {
			p.AvgDelay1H = sum / float64(valid)
		}
		p.SuccessMinutes = valid
		if p.SampleMinutes > 0 {
			p.LossRate1H = lossSum / float64(p.SampleMinutes)
		}
		if value, ok := current[task.Name]; ok {
			p.HasCurrent = true
			p.CurrentDelay = value
			p.CurrentAt = st.LastReport
		}
		out = append(out, p)
	}
	return out
}

func reportFreshness(seconds int) time.Duration {
	if seconds < 2 || seconds > 60 {
		seconds = 3
	}
	return max(90*time.Second, time.Duration(seconds*3)*time.Second)
}

func statusIsFresh(st common.ServerStatus, seconds int, now time.Time) bool {
	return st.IsOnline && st.LastReport > 0 && now.Sub(time.Unix(st.LastReport, 0)) <= reportFreshness(seconds)
}
