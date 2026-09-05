package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	_ "modernc.org/sqlite"
	"my-vps-probe/common"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type AppConfig struct {
	SiteName             string              `json:"site_name"`
	AdminUser            string              `json:"admin_user"`
	AdminPass            string              `json:"admin_pass"`
	Nodes                []common.NodeConfig `json:"nodes"`
	PingTasks            []common.PingTask   `json:"ping_tasks"`
	Telegram             TelegramConfig      `json:"telegram"`
	HistoryDays          int                 `json:"history_days"`
	PublicRefreshSeconds int                 `json:"public_refresh_seconds"`
	AgentReportSeconds   int                 `json:"agent_report_seconds"`
}

var (
	serverStatusMap   = make(map[string]common.ServerStatus)
	activeConns       = make(map[string]*agentConnection)
	connMutex         sync.Mutex
	appConfig         AppConfig
	configMutex       sync.RWMutex
	mapMutex          sync.RWMutex
	db                *sql.DB
	backgroundStop    = make(chan struct{})
	backgroundWorkers sync.WaitGroup
	connectionWorkers sync.WaitGroup
)

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		configMutex.RLock()
		expectedUser := appConfig.AdminUser
		expectedPass := appConfig.AdminPass
		configMutex.RUnlock()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
func initDB() {
	var err error
	db, err = sql.Open("sqlite", "file:data.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			log.Printf("database pragma failed: %v", err)
		}
	}

	mustExecDB(`CREATE TABLE IF NOT EXISTS ping_history (
id INTEGER PRIMARY KEY AUTOINCREMENT,
timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
server_id TEXT,
target_name TEXT,
delay REAL,
loss_rate REAL
);`)

	mustExecDB(`CREATE TABLE IF NOT EXISTS resource_history (
id INTEGER PRIMARY KEY AUTOINCREMENT,
timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
server_id TEXT,
cpu_usage REAL,
mem_used INTEGER,
mem_total INTEGER,
disk_used INTEGER,
disk_total INTEGER,
swap_used INTEGER,
swap_total INTEGER,
load_1 REAL,
net_in_speed INTEGER,
net_out_speed INTEGER,
	tcp_connections INTEGER,
	udp_connections INTEGER
);`)

	ensureResourceHistoryColumns()
	mustExecDB(`CREATE INDEX IF NOT EXISTS idx_ping_history_server_time
ON ping_history(server_id, timestamp);`)
	mustExecDB(`CREATE INDEX IF NOT EXISTS idx_resource_history_server_time
ON resource_history(server_id, timestamp);`)

	mustExecDB(`CREATE TABLE IF NOT EXISTS notification_events (
event_key TEXT PRIMARY KEY,
sent_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`)

	mustExecDB(`CREATE TABLE IF NOT EXISTS notification_log (
 id INTEGER PRIMARY KEY AUTOINCREMENT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
 node_id TEXT, kind TEXT, status TEXT, message TEXT, error TEXT
);`)

	backgroundWorkers.Add(1)
	go func() {
		defer backgroundWorkers.Done()
		// Align archive points to wall-clock minutes. Resource history is saved
		// first so a temporary Ping backlog cannot starve all resource charts.
		minuteTimer := time.NewTimer(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
		cleanupTicker := time.NewTicker(time.Hour)
		defer minuteTimer.Stop()
		defer cleanupTicker.Stop()
		for {
			select {
			case <-backgroundStop:
				return
			case tick := <-minuteTimer.C:
				saveHistoryToDB()
				flushPingMinutes(tick, false)
				flushMonthlyUsage()
				minuteTimer.Reset(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
			case <-cleanupTicker.C:
				cleanupHistory()
			}
		}
	}()
}

func mustExecDB(query string, args ...interface{}) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Fatalf("database init failed: %v", err)
	}
}

func cleanupHistory() {
	configMutex.RLock()
	days := appConfig.HistoryDays
	configMutex.RUnlock()
	if days < 1 || days > 365 {
		days = 7
	}
	cutoff := fmt.Sprintf("-%d days", days)
	if _, err := db.Exec("DELETE FROM ping_history WHERE timestamp <= datetime('now', ?)", cutoff); err != nil {
		log.Printf("cleanup ping history: %v", err)
	}
	if _, err := db.Exec("DELETE FROM resource_history WHERE timestamp <= datetime('now', ?)", cutoff); err != nil {
		log.Printf("cleanup resource history: %v", err)
	}
	_, _ = db.Exec("DELETE FROM notification_events WHERE event_key LIKE 'expiry:%' AND sent_at <= datetime('now', '-7 days')")
	_, _ = db.Exec("DELETE FROM notification_log WHERE created_at < datetime('now', '-7 days') OR id NOT IN (SELECT id FROM notification_log ORDER BY id DESC LIMIT 1000)")
}

func loadConfig() {
	data, err := os.ReadFile("config.json")
	isNew := err != nil
	if err == nil {
		if err := json.Unmarshal(data, &appConfig); err != nil {
			log.Fatalf("config.json 格式错误: %v", err)
		}
	} else {
		appConfig = AppConfig{
			SiteName: "探针看板", AdminUser: "admin", AdminPass: "123456",
			HistoryDays: 7, PublicRefreshSeconds: 3, AgentReportSeconds: 3,
			Nodes:     []common.NodeConfig{{ID: "node-1", Name: "主控测试机", Token: "my_secret_token_123", ExpireDate: "2027/05/13", Region: "CN"}},
			PingTasks: []common.PingTask{{Name: "广东电信", Host: "gd-ct-v4.ip.zstaticcdn.com:80"}, {Name: "广东联通", Host: "gd-cu-v4.ip.zstaticcdn.com:80"}, {Name: "广东移动", Host: "gd-cm-v4.ip.zstaticcdn.com:80"}},
		}
	}
	if appConfig.AdminUser == "" {
		appConfig.AdminUser = "admin"
	}
	if appConfig.AdminPass == "" {
		appConfig.AdminPass = "123456"
	}
	if appConfig.SiteName == "" {
		appConfig.SiteName = "探针看板"
	}
	if isNew && len(appConfig.PingTasks) == 0 {
		appConfig.PingTasks = []common.PingTask{{Name: "广东电信", Host: "gd-ct-v4.ip.zstaticcdn.com:80"}, {Name: "广东联通", Host: "gd-cu-v4.ip.zstaticcdn.com:80"}, {Name: "广东移动", Host: "gd-cm-v4.ip.zstaticcdn.com:80"}}
	}
	normalizeAppConfig(&appConfig)
	if err := saveAppConfig(appConfig); err != nil {
		log.Fatalf("保存规范化配置失败: %v", err)
	}
}

func normalizeAppConfig(c *AppConfig) {
	if c.HistoryDays < 1 || c.HistoryDays > 365 {
		c.HistoryDays = 7
	}
	if c.PublicRefreshSeconds < 2 || c.PublicRefreshSeconds > 60 {
		c.PublicRefreshSeconds = 3
	}
	if c.AgentReportSeconds < 2 || c.AgentReportSeconds > 60 {
		c.AgentReportSeconds = 3
	}
	c.Telegram.normalize()
}

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" { // Agent clients do not send a browser Origin header.
			return true
		}
		u, err := url.Parse(origin)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	},
}

func pingTasksForNode(tasks []common.PingTask, nodeID string) []common.PingTask {
	out := make([]common.PingTask, 0, len(tasks))
	for _, t := range tasks {
		if t.NodeIDs == nil {
			out = append(out, t)
			continue
		}
		for _, id := range t.NodeIDs {
			if id == nodeID {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

func main() {
	loadConfig()
	initDB()
	startNotificationWorker()
	defer db.Close()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "server/index.html")
	})
	http.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "server/admin.html")
	})
	http.HandleFunc("/probe-agent-amd64", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "server/probe-agent-amd64")
	})
	http.HandleFunc("/probe-agent-arm64", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "server/probe-agent-arm64")
	})
	http.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "install.sh")
	})
	http.HandleFunc("/download/agent.go", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "agent/main.go") })
	http.HandleFunc("/api/admin/traffic", basicAuth(trafficCalibrationHandler))
	http.HandleFunc("/api/admin/config", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			configMutex.RLock()
			safeConfig := appConfig
			safeConfig.Nodes = append([]common.NodeConfig(nil), appConfig.Nodes...)
			safeConfig.PingTasks = append([]common.PingTask(nil), appConfig.PingTasks...)
			safeConfig.AdminPass = ""
			safeConfig.Telegram.Token = ""
			configMutex.RUnlock()
			writeJSON(w, http.StatusOK, safeConfig)
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			var newConfig AppConfig
			if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
				writeAPIError(w, http.StatusBadRequest, "配置格式无效："+err.Error())
				return
			}

			configMutex.Lock()
			if newConfig.AdminPass == "" {
				newConfig.AdminPass = appConfig.AdminPass
			}
			if newConfig.Telegram.Token == "" {
				newConfig.Telegram.Token = appConfig.Telegram.Token
			}
			if err := validateConfig(&newConfig); err != nil {
				configMutex.Unlock()
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			if err := saveAppConfig(newConfig); err != nil {
				configMutex.Unlock()
				log.Printf("save config: %v", err)
				writeAPIError(w, http.StatusInternalServerError, "配置写入失败")
				return
			}
			appConfig = newConfig
			pTasks := append([]common.PingTask(nil), appConfig.PingTasks...)
			reportSeconds := appConfig.AgentReportSeconds
			nodeNameMap := map[string]string{}
			for _, n := range appConfig.Nodes {
				nodeNameMap[n.ID] = n.Name
			}
			configMutex.Unlock()
			mapMutex.Lock()
			for id := range serverStatusMap {
				if _, exists := nodeNameMap[id]; !exists {
					delete(serverStatusMap, id)
				}
			}
			mapMutex.Unlock()
			invalidateCardPingCache()

			// Hot-push settings without forcing healthy agents to reconnect.
			connMutex.Lock()
			connections := make(map[string]*agentConnection, len(activeConns))
			for id, conn := range activeConns {
				connections[id] = conn
			}
			connMutex.Unlock()
			var pushWG sync.WaitGroup
			pushLimit := make(chan struct{}, 16)
			for id, conn := range connections {
				pushWG.Add(1)
				go func(id string, conn *agentConnection) {
					defer pushWG.Done()
					pushLimit <- struct{}{}
					defer func() { <-pushLimit }()
					name, ok := nodeNameMap[id]
					if !ok {
						removeActiveConnection(id, conn)
						_ = conn.close()
						return
					}
					instruction := common.AgentInstruction{ServerName: name, PingTasks: pingTasksForNode(pTasks, id), ReportSeconds: reportSeconds}
					if err := conn.writeJSON(instruction); err != nil {
						removeActiveConnection(id, conn)
						_ = conn.close()
						markNodeOffline(id)
					}
				}(id, conn)
			}
			pushWG.Wait()
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	}))
	http.HandleFunc("/api/admin/runtime", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		writeJSON(w, http.StatusOK, adminRuntimeSnapshot())
	}))
	http.HandleFunc("/api/admin/notifications", basicAuth(notificationLogHandler))
	http.HandleFunc("/api/admin/telegram/test", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		configMutex.RLock()
		tg := appConfig.Telegram
		configMutex.RUnlock()
		if !tg.Enabled || tg.Token == "" || tg.ChatID == "" {
			http.Error(w, "请先启用 TG 通知，并填写 Bot Token 与 Chat ID 后保存", http.StatusBadRequest)
			return
		}
		testMessage := "✅ My VPS Probe 测试通知\nTG 通知配置成功。"
		sendErr := sendTelegram(tg, testMessage)
		recordNotificationDelivery([]notification{{Kind: "test", Message: testMessage}}, sendErr, time.Now())
		if sendErr != nil {
			http.Error(w, sendErr.Error(), http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		configMutex.RLock()
		var cNode common.NodeConfig
		found := false
		for _, n := range appConfig.Nodes {
			if n.Token == token {
				cNode = n
				found = true
				break
			}
		}
		pTasks := append([]common.PingTask(nil), appConfig.PingTasks...)
		reportSeconds := appConfig.AgentReportSeconds
		configMutex.RUnlock()
		if !found {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		rawConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connectionWorkers.Add(1)
		defer connectionWorkers.Done()
		conn := &agentConnection{conn: rawConn}
		defer conn.close()
		connMutex.Lock()
		if old := activeConns[cNode.ID]; old != nil && old != conn {
			_ = old.close()
		}
		activeConns[cNode.ID] = conn
		connMutex.Unlock()
		defer func() {
			connMutex.Lock()
			if activeConns[cNode.ID] == conn {
				delete(activeConns, cNode.ID)
				markNodeOffline(cNode.ID)
			}
			connMutex.Unlock()
		}()
		writeErr := conn.writeJSON(common.AgentInstruction{ServerName: cNode.Name, PingTasks: pingTasksForNode(pTasks, cNode.ID), ReportSeconds: reportSeconds})
		if writeErr != nil {
			return
		}
		mapMutex.Lock()
		st := serverStatusMap[cNode.ID]
		st.IsOnline = true
		serverStatusMap[cNode.ID] = st
		mapMutex.Unlock()
		conn.conn.SetReadLimit(2 << 20)
		for {
			// Decode into a new value so readers never share a slice being mutated.
			var next common.ServerStatus
			_ = conn.conn.SetReadDeadline(time.Now().Add(reportFreshness(reportSeconds)))
			if err := conn.conn.ReadJSON(&next); err != nil {
				connMutex.Lock()
				isCurrent := activeConns[cNode.ID] == conn
				connMutex.Unlock()
				if isCurrent {
					markNodeOffline(cNode.ID)
				}
				break
			}
			now := time.Now()
			next, corrected := sanitizeReportedStatus(next, st)
			logStatusCorrections(cNode.ID, corrected, now)
			configMutex.RLock()
			currentNode, valid := configuredNode(appConfig.Nodes, cNode.ID, token)
			tg := appConfig.Telegram
			reportSeconds = appConfig.AgentReportSeconds
			if !valid {
				configMutex.RUnlock()
				break
			}
			// Serialize a replacement connection with its last report. Keep the
			// quota configuration stable until the corresponding counters commit.
			connMutex.Lock()
			if activeConns[cNode.ID] != conn {
				connMutex.Unlock()
				configMutex.RUnlock()
				break
			}
			st = next
			st.ServerID = cNode.ID
			st.IsOnline = true
			st.LastReport = now.Unix()
			observeMonthlyUsage(currentNode.ID, currentNode.ExpireDate, st, now)
			mapMutex.Lock()
			serverStatusMap[cNode.ID] = st
			mapMutex.Unlock()
			connMutex.Unlock()
			configMutex.RUnlock()
			recordPingSamples(cNode.ID, st.PingStatuses, now)
			resourceMonitor.observe(currentNode, st, tg, reportSeconds, now)
		}
	})
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		configMutex.RLock()
		nodeConfigs := append([]common.NodeConfig(nil), appConfig.Nodes...)
		pingTasks := append([]common.PingTask(nil), appConfig.PingTasks...)
		siteName := appConfig.SiteName
		refreshSeconds := appConfig.PublicRefreshSeconds
		reportSeconds := appConfig.AgentReportSeconds
		configMutex.RUnlock()
		mapMutex.RLock()
		statuses := make(map[string]common.ServerStatus, len(serverStatusMap))
		for id, status := range serverStatusMap {
			statuses[id] = status
		}
		mapMutex.RUnlock()

		now := time.Now()
		cardNodeIDs := make([]string, 0, len(nodeConfigs))
		for _, node := range nodeConfigs {
			cardNodeIDs = append(cardNodeIDs, node.ID)
		}
		primeCardPingHistory(cardNodeIDs, now)
		nodes := make([]FrontendNode, 0, len(nodeConfigs))
		for _, n := range nodeConfigs {
			st := statuses[n.ID]
			st.IsOnline = statusIsFresh(st, reportSeconds, now)
			st.CardPingStatuses = cardPingStatuses(n.ID, pingTasksForNode(pingTasks, n.ID), st, reportSeconds, now)
			nodes = append(nodes, buildFrontendNode(n, st))
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"site_name": siteName, "nodes": nodes, "ping_tasks": pingTasks,
			"refresh_seconds": refreshSeconds, "updated_at": now.Unix(),
		})
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "resource_history": resourceHistorySnapshot()})
	})
	http.HandleFunc("/api/ping_history", trendHistoryHandler("ping"))
	http.HandleFunc("/api/resource_history", trendHistoryHandler("resource"))
	fmt.Println("🚀 My VPS Probe 已启动，监听 :8080")
	server := &http.Server{
		Addr:              ":8080",
		Handler:           securityHeaders(gzipTextResponses(http.DefaultServeMux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		close(backgroundStop)
		backgroundWorkers.Wait()
		connMutex.Lock()
		for _, conn := range activeConns {
			_ = conn.close()
		}
		connMutex.Unlock()
		connectionWorkers.Wait()
		flushPingMinutes(time.Now(), true)
		flushMonthlyUsage()
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}
