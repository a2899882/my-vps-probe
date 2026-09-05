package main

import (
	"database/sql"
	"log"
	"math"
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
	tx, err := db.Begin()
	if err != nil {
		log.Printf("begin ping history: %v", err)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO ping_history(timestamp,server_id,target_name,delay,loss_rate) VALUES(?,?,?,?,?)`)
	if err != nil {
		log.Printf("prepare ping history: %v", err)
		return
	}
	defer stmt.Close()
	for k, v := range snapshot {
		delay, loss := v.values()
		if _, err := stmt.Exec(time.Unix(k.Minute*60, 0).UTC().Format("2006-01-02 15:04:05"), k.Server, k.Target, delay, loss); err != nil {
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
	Minute int64
	Points []pingHistoryPoint
}

var pingHistoryCache = struct {
	sync.Mutex
	items map[string]cachedPingHistory
}{items: make(map[string]cachedPingHistory)}

func invalidateCardPingCache() {
	pingHistoryCache.Lock()
	pingHistoryCache.items = make(map[string]cachedPingHistory)
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

func cardPingStatuses(id string, tasks []common.PingTask, st common.ServerStatus, reportSeconds int, now time.Time) []common.CardPingStatus {
	// A small per-minute cache avoids N SQL queries on every dashboard poll.
	// Serialize cache reads with buffer removal so no snapshot falls in between.
	pingHistoryCache.Lock()
	entry, ok := pingHistoryCache.items[id]
	if !ok || entry.Minute != now.Unix()/60 {
		points, err := readCardPingHistory(db, id, now)
		if err != nil {
			log.Printf("read card ping history: %v", err)
		} else {
			entry = cachedPingHistory{Minute: now.Unix() / 60, Points: points}
			pingHistoryCache.items[id] = entry
		}
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
	return buildCardPingStatuses(id, tasks, entry.Points, buffer, st, reportSeconds, now)
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
