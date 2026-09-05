package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"my-vps-probe/common"
)

type TelegramConfig struct {
	Enabled            bool                `json:"enabled"`
	Token              string              `json:"token"`
	ChatID             string              `json:"chat_id"`
	OfflineMinutes     int                 `json:"offline_minutes"`
	ExpiryReminderDays int                 `json:"expiry_reminder_days"`
	NotifyOffline      *bool               `json:"notify_offline"`
	NotifyExpiry       *bool               `json:"notify_expiry"`
	NotifyRecovery     *bool               `json:"notify_recovery"`
	RepeatMinutes      int                 `json:"repeat_minutes"`
	RecoverySeconds    int                 `json:"recovery_seconds"`
	ExcludedNodeIDs    []string            `json:"excluded_node_ids"`
	Resources          ResourceAlertConfig `json:"resources"`
}

func (t *TelegramConfig) normalize() {
	if t.OfflineMinutes < 1 {
		t.OfflineMinutes = 3
	}
	t.OfflineMinutes = min(t.OfflineMinutes, 10080)
	if t.ExpiryReminderDays < 1 {
		t.ExpiryReminderDays = 3
	}
	t.ExpiryReminderDays = min(t.ExpiryReminderDays, 365)
	for _, p := range []**bool{&t.NotifyOffline, &t.NotifyExpiry, &t.NotifyRecovery} {
		if *p == nil {
			v := true
			*p = &v
		}
	}
	t.RepeatMinutes = max(0, min(10080, t.RepeatMinutes))
	if t.RecoverySeconds < 15 {
		t.RecoverySeconds = 60
	}
	t.RecoverySeconds = min(3600, t.RecoverySeconds)
	t.Resources.CPU.normalize(90, 180)
	t.Resources.Memory.normalize(90, 60)
	t.Resources.Disk.normalize(90, 300)
	t.Resources.Traffic.normalize(80, 0)
	t.Token = strings.TrimSpace(t.Token)
	t.ChatID = strings.TrimSpace(t.ChatID)
	if t.ExcludedNodeIDs == nil {
		t.ExcludedNodeIDs = []string{}
	}
}
func boolEnabled(p *bool) bool       { return p == nil || *p }
func (t TelegramConfig) ready() bool { return t.Enabled && t.Token != "" && t.ChatID != "" }
func (t TelegramConfig) excludes(id string) bool {
	for _, n := range t.ExcludedNodeIDs {
		if n == id {
			return true
		}
	}
	return false
}

func startNotificationWorker() {
	events, err := loadNotificationEvents()
	if err != nil {
		log.Printf("load notification states: %v", err)
	} else {
		resourceMonitor.restore(events)
	}
	backgroundWorkers.Add(1)
	go func() {
		defer backgroundWorkers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runNotifications()
			case <-backgroundStop:
				return
			}
		}
	}()
}

type notification struct {
	Key, NodeID, Kind, Message string
	Recovery                   bool
}

var notificationNextAttempt time.Time // Accessed only by the single notification worker.

func loadNotificationEvents() (map[string]time.Time, error) {
	rows, err := db.Query(`SELECT event_key,unixepoch(sent_at) FROM notification_events`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make(map[string]time.Time)
	for rows.Next() {
		var key string
		var ts int64
		if err := rows.Scan(&key, &ts); err != nil {
			return nil, err
		}
		events[key] = time.Unix(ts, 0)
	}
	return events, rows.Err()
}
func reminderDue(last time.Time, repeat int, now time.Time) bool {
	return last.IsZero() || (repeat > 0 && now.Sub(last) >= time.Duration(repeat)*time.Minute)
}

func collectNotifications(tg TelegramConfig, nodes []common.NodeConfig, statuses map[string]common.ServerStatus, states map[string]resourceAlertState, events map[string]time.Time, seconds int, now time.Time) ([]notification, []string) {
	var pending []notification
	var clear []string
	allowed := make(map[string]bool)
	for _, n := range nodes {
		if tg.excludes(n.ID) || !tg.ready() {
			continue
		}
		s := statuses[n.ID]
		offlineKey := "offline:" + n.ID
		if boolEnabled(tg.NotifyOffline) {
			allowed[offlineKey] = true
			isOffline := s.LastReport > 0 && now.Sub(time.Unix(s.LastReport, 0)) >= time.Duration(tg.OfflineMinutes)*time.Minute
			if isOffline && reminderDue(events[offlineKey], tg.RepeatMinutes, now) {
				pending = append(pending, notification{Key: offlineKey, NodeID: n.ID, Kind: "offline", Message: fmt.Sprintf("🔴 节点离线\n名称：%s\n分组：%s\n离线阈值：%d 分钟\n最后上报：%s", n.Name, displayGroup(n.Group), tg.OfflineMinutes, time.Unix(s.LastReport, 0).Format("2006-01-02 15:04:05"))})
			} else if !events[offlineKey].IsZero() && statusIsFresh(s, seconds, now) {
				if boolEnabled(tg.NotifyRecovery) {
					pending = append(pending, notification{Key: offlineKey, NodeID: n.ID, Kind: "online", Recovery: true, Message: fmt.Sprintf("🟢 节点恢复上线\n名称：%s\n分组：%s\n恢复时间：%s", n.Name, displayGroup(n.Group), now.Format("2006-01-02 15:04:05"))})
				} else {
					clear = append(clear, offlineKey)
				}
			}
		}
		if boolEnabled(tg.NotifyExpiry) {
			if expiry, ok := parseNodeExpireDate(n.ExpireDate); ok {
				today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
				days := int(expiry.Sub(today).Hours() / 24)
				key := fmt.Sprintf("expiry:%s:%s", n.ID, today.Format("2006-01-02"))
				if days >= 0 && days <= tg.ExpiryReminderDays && events[key].IsZero() {
					remaining := fmt.Sprintf("剩余：%d 天", days)
					if days == 0 {
						remaining = "今天到期，请及时续费。"
					}
					pending = append(pending, notification{Key: key, NodeID: n.ID, Kind: "expiry", Message: fmt.Sprintf("⚠️ 服务器到期提醒\n名称：%s\n分组：%s\n到期日期：%s\n%s", n.Name, displayGroup(n.Group), expiry.Format("2006/01/02"), remaining)})
				}
			}
		}
		rules := tg.Resources.rules()
		for _, kind := range []string{"cpu", "memory", "disk", "traffic"} {
			rule := rules[kind]
			key := resourceKey(n.ID, kind)
			_, quota, _ := parseNodeQuota(n.ExpireDate)
			if !rule.Enabled || (kind == "traffic" && quota <= 0) {
				continue
			}
			allowed[key] = true
			state, ok := states[key]
			if !ok || !state.Valid || !statusIsFresh(s, seconds, now) || now.Sub(state.LastReport) > reportFreshness(seconds) {
				continue
			}
			if state.Firing && (state.Value >= rule.Threshold || !events[key].IsZero()) && reminderDue(events[key], tg.RepeatMinutes, now) {
				duration := max(0, int(now.Sub(state.Since).Seconds()))
				if state.Since.IsZero() {
					duration = 0
				}
				pending = append(pending, notification{Key: key, NodeID: n.ID, Kind: kind, Message: fmt.Sprintf("🔴 %s告警\n名称：%s\n分组：%s\n当前：%.1f%%（%s）\n阈值：≥ %.1f%%，持续 ≥ %d 秒\n本次持续：%d 秒", resourceNames[kind], n.Name, displayGroup(n.Group), state.Value, state.Detail, rule.Threshold, rule.DurationSeconds, duration)})
			} else if !state.Firing && !events[key].IsZero() {
				if boolEnabled(tg.NotifyRecovery) {
					pending = append(pending, notification{Key: key, NodeID: n.ID, Kind: kind + "_recovery", Recovery: true, Message: fmt.Sprintf("🟢 %s恢复\n名称：%s\n分组：%s\n当前：%.1f%%（%s）\n已低于恢复阈值并持续 %d 秒", resourceNames[kind], n.Name, displayGroup(n.Group), state.Value, state.Detail, tg.RecoverySeconds)})
				} else {
					clear = append(clear, key)
				}
			}
		}
	}
	for key := range events {
		if !strings.HasPrefix(key, "expiry:") && !allowed[key] {
			clear = append(clear, key)
		}
	}
	for key := range states {
		if !allowed[key] {
			resourceMonitor.discard(key)
		}
	}
	return pending, clear
}

func runNotifications() {
	configMutex.RLock()
	tg := appConfig.Telegram
	nodes := append([]common.NodeConfig(nil), appConfig.Nodes...)
	seconds := appConfig.AgentReportSeconds
	site := appConfig.SiteName
	configMutex.RUnlock()
	events, err := loadNotificationEvents()
	if err != nil {
		log.Printf("read notification events: %v", err)
		return
	}
	mapMutex.RLock()
	statuses := make(map[string]common.ServerStatus, len(serverStatusMap))
	for k, v := range serverStatusMap {
		statuses[k] = v
	}
	mapMutex.RUnlock()
	now := time.Now()
	pending, clear := collectNotifications(tg, nodes, statuses, resourceMonitor.snapshot(), events, seconds, now)
	for _, key := range clear {
		releaseEvent(key)
	}
	if len(pending) == 0 || now.Before(notificationNextAttempt) {
		return
	}
	// One merged message per five-second tick (at most 12/minute per chat). Events
	// beyond this batch remain unclaimed for the next tick, not silently dropped.
	batch, text := notificationBatch(site, pending)
	err = sendTelegram(tg, text)
	notificationNextAttempt = now.Add(5 * time.Second)
	if err != nil {
		wait := 30 * time.Second
		var retry *telegramRetryError
		if errors.As(err, &retry) {
			wait = max(wait, time.Duration(retry.Seconds)*time.Second)
		}
		notificationNextAttempt = time.Now().Add(wait)
		log.Printf("Telegram notification failed: %v", err)
	}
	recordNotificationDelivery(batch, err, time.Now())
}

func notificationBatch(site string, pending []notification) ([]notification, string) {
	text := "📡 " + site + "\n"
	var batch []notification
	for _, n := range pending {
		next := text + "\n" + n.Message + "\n"
		if len(utf16.Encode([]rune(next))) > 3800 {
			break
		}
		text = next
		batch = append(batch, n)
	}
	return batch, text
}

func recordNotificationDelivery(batch []notification, sendErr error, now time.Time) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("record notification: %v", err)
		return
	}
	defer tx.Rollback()
	status, detail := "sent", ""
	if sendErr != nil {
		status = "failed"
		detail = sendErr.Error()
	}
	for _, n := range batch {
		if sendErr == nil && n.Key != "" {
			if n.Recovery {
				_, err = tx.Exec(`DELETE FROM notification_events WHERE event_key=?`, n.Key)
			} else {
				_, err = tx.Exec(`INSERT INTO notification_events(event_key,sent_at) VALUES(?,?) ON CONFLICT(event_key) DO UPDATE SET sent_at=excluded.sent_at`, n.Key, now.UTC().Format("2006-01-02 15:04:05"))
			}
			if err != nil {
				log.Printf("save notification acknowledgement: %v", err)
				return
			}
		}
		if _, err = tx.Exec(`INSERT INTO notification_log(node_id,kind,status,message,error) VALUES(?,?,?,?,?)`, n.NodeID, n.Kind, status, n.Message, detail); err != nil {
			log.Printf("save notification log: %v", err)
			return
		}
	}
	// A hard row cap also applies during an alert storm, between hourly cleanups.
	if _, err = tx.Exec(`DELETE FROM notification_log WHERE id NOT IN (SELECT id FROM notification_log ORDER BY id DESC LIMIT 1000)`); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		log.Printf("commit notification delivery: %v", err)
	}
}

func notificationLogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method Not Allowed")
		return
	}
	rows, err := db.Query(`SELECT id,unixepoch(created_at),node_id,kind,status,message,error FROM notification_log ORDER BY id DESC LIMIT 50`)
	if err != nil {
		writeAPIError(w, 500, "读取通知记录失败")
		return
	}
	defer rows.Close()
	type entry struct {
		ID      int64  `json:"id"`
		Time    int64  `json:"time"`
		NodeID  string `json:"node_id"`
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	items := make([]entry, 0)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.Time, &e.NodeID, &e.Kind, &e.Status, &e.Message, &e.Error); err != nil {
			writeAPIError(w, 500, "读取通知记录失败")
			return
		}
		items = append(items, e)
	}
	if rows.Err() != nil {
		writeAPIError(w, 500, "读取通知记录失败")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items})
}

func displayGroup(group string) string {
	if strings.TrimSpace(group) == "" {
		return "未分组"
	}
	return group
}
func parseNodeExpireDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(strings.Split(raw, "|")[0])
	for _, layout := range []string{"2006/01/02", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func releaseEvent(key string) {
	if _, err := db.Exec(`DELETE FROM notification_events WHERE event_key=?`, key); err != nil {
		log.Printf("clear notification event: %v", err)
	}
}

type telegramRetryError struct{ Seconds int }

func (e *telegramRetryError) Error() string {
	return fmt.Sprintf("Telegram 限流，%d 秒后重试", e.Seconds)
}

var telegramClient = &http.Client{Timeout: 12 * time.Second}

func sendTelegram(tg TelegramConfig, text string) error {
	body, _ := json.Marshal(map[string]string{"chat_id": tg.ChatID, "text": text})
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(tg.Token) + "/sendMessage"
	resp, err := telegramClient.Post(endpoint, "application/json", bytes.NewReader(body))
	// net/http errors include the request URL (and therefore the Bot Token).
	if err != nil {
		return errors.New("无法连接 Telegram，请检查主控网络或稍后重试")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return errors.New("读取 Telegram 响应失败")
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &result) != nil {
		return fmt.Errorf("Telegram 响应格式无效（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &telegramRetryError{Seconds: max(1, result.Parameters.RetryAfter)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		description := result.Description
		if tg.Token != "" {
			description = strings.ReplaceAll(description, tg.Token, "[已隐藏]")
		}
		return fmt.Errorf("Telegram 发送失败（HTTP %d）：%s", resp.StatusCode, trimLimited(description, 300))
	}
	return nil
}
