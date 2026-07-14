package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"my-vps-probe/common"
)

type TelegramConfig struct {
	Enabled            bool   `json:"enabled"`
	Token              string `json:"token"`
	ChatID             string `json:"chat_id"`
	OfflineMinutes     int    `json:"offline_minutes"`
	ExpiryReminderDays int    `json:"expiry_reminder_days"`
}

func (t *TelegramConfig) normalize() {
	if t.OfflineMinutes < 1 {
		t.OfflineMinutes = 3
	}
	if t.OfflineMinutes > 10080 {
		t.OfflineMinutes = 10080
	}
	if t.ExpiryReminderDays < 1 {
		t.ExpiryReminderDays = 3
	}
	if t.ExpiryReminderDays > 365 {
		t.ExpiryReminderDays = 365
	}
	t.Token = strings.TrimSpace(t.Token)
	t.ChatID = strings.TrimSpace(t.ChatID)
}

func startNotificationWorker() {
	go func() {
		runNotifications()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			runNotifications()
		}
	}()
}

func runNotifications() {
	configMutex.RLock()
	tg := appConfig.Telegram
	nodes := append([]common.NodeConfig(nil), appConfig.Nodes...)
	configMutex.RUnlock()
	tg.normalize()
	if !tg.Enabled || tg.Token == "" || tg.ChatID == "" {
		return
	}

	mapMutex.RLock()
	statuses := make(map[string]common.ServerStatus, len(serverStatusMap))
	for id, st := range serverStatusMap {
		statuses[id] = st
	}
	mapMutex.RUnlock()

	now := time.Now()
	threshold := time.Duration(tg.OfflineMinutes) * time.Minute

	for _, node := range nodes {
		st := statuses[node.ID]
		offlineKey := "offline:" + node.ID
		// 仅在节点曾成功上报、且最后上报距今达到后台配置阈值时告警。
		// WebSocket 瞬时断开不会立即推送 TG，避免短暂抖动造成误报。
		isOffline := st.LastReport > 0 && now.Sub(time.Unix(st.LastReport, 0)) >= threshold

		if isOffline {
			if claimEvent(offlineKey) {
				last := "从未上报"
				if st.LastReport > 0 {
					last = time.Unix(st.LastReport, 0).Format("2006-01-02 15:04:05")
				}
				msg := fmt.Sprintf("🔴 节点离线\n名称：%s\n分组：%s\n离线阈值：%d 分钟\n最后上报：%s",
					node.Name, displayGroup(node.Group), tg.OfflineMinutes, last)
				if err := sendTelegram(tg, msg); err != nil {
					releaseEvent(offlineKey)
				}
			}
		} else if removeEvent(offlineKey) {
			msg := fmt.Sprintf("🟢 节点恢复上线\n名称：%s\n分组：%s\n恢复时间：%s",
				node.Name, displayGroup(node.Group), now.Format("2006-01-02 15:04:05"))
			_ = sendTelegram(tg, msg)
		}

		if expireDate, ok := parseNodeExpireDate(node.ExpireDate); ok {
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			days := int(expireDate.Sub(today).Hours() / 24)
			if days >= 1 && days <= tg.ExpiryReminderDays {
				key := fmt.Sprintf("expiry:%s:%s", node.ID, today.Format("2006-01-02"))
				if claimEvent(key) {
					msg := fmt.Sprintf("⚠️ 服务器到期提醒\n名称：%s\n分组：%s\n到期日期：%s\n剩余：%d 天\n请及时续费。",
						node.Name, displayGroup(node.Group), expireDate.Format("2006/01/02"), days)
					if err := sendTelegram(tg, msg); err != nil {
						releaseEvent(key)
					}
				}
			}
		}
	}
}

func displayGroup(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
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

func claimEvent(key string) bool {
	res, err := db.Exec(`INSERT OR IGNORE INTO notification_events(event_key) VALUES(?)`, key)
	return err == nil && func() bool { n, _ := res.RowsAffected(); return n == 1 }()
}

func removeEvent(key string) bool {
	res, err := db.Exec(`DELETE FROM notification_events WHERE event_key = ?`, key)
	return err == nil && func() bool { n, _ := res.RowsAffected(); return n > 0 }()
}

func releaseEvent(key string) {
	_, _ = db.Exec(`DELETE FROM notification_events WHERE event_key = ?`, key)
}

func sendTelegram(tg TelegramConfig, text string) error {
	body, _ := json.Marshal(map[string]string{
		"chat_id": tg.ChatID,
		"text":    text,
	})
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(tg.Token) + "/sendMessage"
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram 返回 %s：%s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(data, &result) != nil || !result.OK {
		return fmt.Errorf("Telegram 未确认发送成功")
	}
	return nil
}

func _keepStrconv() { _ = strconv.IntSize }
