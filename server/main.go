package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	_ "modernc.org/sqlite"
	"my-vps-probe/common"
	"net/http"
	"os"
	"sync"
	"time"
)

type AppConfig struct {
	SiteName  string              `json:"site_name"`
	AdminUser string              `json:"admin_user"`
	AdminPass string              `json:"admin_pass"`
	Nodes     []common.NodeConfig `json:"nodes"`
	PingTasks []common.PingTask   `json:"ping_tasks"`
}

var (
	serverStatusMap = make(map[string]common.ServerStatus)
	activeConns     = make(map[string]*websocket.Conn) // 【新增】连接池
	connMutex       sync.Mutex                         // 保护连接池
	appConfig       AppConfig
	configMutex     sync.RWMutex
	mapMutex        sync.RWMutex
	db              *sql.DB
)

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		configMutex.RLock()
		expectedUser := appConfig.AdminUser
		expectedPass := appConfig.AdminPass
		configMutex.RUnlock()
		if !ok || user != expectedUser || pass != expectedPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
func initDB() {
	db, _ = sql.Open("sqlite", "data.db")
	db.Exec(`CREATE TABLE IF NOT EXISTS ping_history (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP, server_id TEXT, target_name TEXT, delay REAL, loss_rate REAL);`)
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			saveHistoryToDB()
			db.Exec("DELETE FROM ping_history WHERE timestamp <= datetime('now', '-3 days')")
		}
	}()
}
func saveHistoryToDB() {
	mapMutex.RLock()
	defer mapMutex.RUnlock()
	tx, _ := db.Begin()
	defer tx.Commit()
	stmt, _ := tx.Prepare("INSERT INTO ping_history (server_id, target_name, delay, loss_rate) VALUES (?, ?, ?, ?)")
	defer stmt.Close()
	for serverID, status := range serverStatusMap {
		if !status.IsOnline {
			continue
		}
		for _, ping := range status.PingStatuses {
			stmt.Exec(serverID, ping.TargetName, ping.CurrentDelay, ping.LossRate)
		}
	}
}
func loadConfig() {
	data, err := os.ReadFile("config.json")
	if err == nil {
		json.Unmarshal(data, &appConfig)
	} else {
		appConfig = AppConfig{SiteName: "探针看板", AdminUser: "admin", AdminPass: "123456", Nodes: []common.NodeConfig{{ID: "node-1", Name: "主控测试机", Token: "my_secret_token_123", ExpireDate: "2027/05/13", Region: "CN"}}, PingTasks: []common.PingTask{{Name: "广东电信", Host: "gd-ct-v4.ip.zstaticcdn.com:80"}, {Name: "广东联通", Host: "gd-cu-v4.ip.zstaticcdn.com:80"}, {Name: "广东移动", Host: "gd-cm-v4.ip.zstaticcdn.com:80"}}}
		data, _ := json.MarshalIndent(appConfig, "", "  ")
		os.WriteFile("config.json", data, 0644)
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
	if len(appConfig.PingTasks) == 0 {
		appConfig.PingTasks = []common.PingTask{{Name: "广东电信", Host: "gd-ct-v4.ip.zstaticcdn.com:80"}, {Name: "广东联通", Host: "gd-cu-v4.ip.zstaticcdn.com:80"}, {Name: "广东移动", Host: "gd-cm-v4.ip.zstaticcdn.com:80"}}
	}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func buildCardPingStatuses(serverID string) []common.CardPingStatus {
	rows, err := db.Query(`SELECT datetime(timestamp, 'localtime'), target_name, delay FROM ping_history WHERE server_id = ? AND timestamp >= datetime('now', '-1 hours') ORDER BY timestamp ASC`, serverID)
	if err != nil {
		return []common.CardPingStatus{}
	}
	defer rows.Close()

	type item struct {
		t      string
		target string
		delay  float64
	}
	var items []item
	targetSet := map[string]bool{}
	for rows.Next() {
		var it item
		rows.Scan(&it.t, &it.target, &it.delay)
		if len(it.t) >= 16 {
			it.t = it.t[:16]
		}
		items = append(items, it)
		targetSet[it.target] = true
	}

	minutes := make([]string, 0, 60)
	now := time.Now().Truncate(time.Minute)
	for i := 59; i >= 0; i-- {
		minutes = append(minutes, now.Add(-time.Duration(i)*time.Minute).Format("2006-01-02 15:04"))
	}

	bucket := map[string]map[string]float64{}
	for _, it := range items {
		if _, ok := bucket[it.target]; !ok {
			bucket[it.target] = map[string]float64{}
		}
		bucket[it.target][it.t] = it.delay
	}

	configMutex.RLock()
	taskOrder := make([]string, 0, len(appConfig.PingTasks))
	for _, t := range appConfig.PingTasks {
		taskOrder = append(taskOrder, t.Name)
		targetSet[t.Name] = true
	}
	configMutex.RUnlock()

	ordered := make([]string, 0, len(targetSet))
	used := map[string]bool{}
	for _, name := range taskOrder {
		if targetSet[name] {
			ordered = append(ordered, name)
			used[name] = true
		}
	}
	for name := range targetSet {
		if !used[name] {
			ordered = append(ordered, name)
		}
	}

	out := make([]common.CardPingStatus, 0, len(ordered))
	for _, tgt := range ordered {
		hist := make([]float64, 0, 60)
		valid := 0
		fail := 0
		sum := 0.0
		seen := 0
		for _, mk := range minutes {
			v, ok := bucket[tgt][mk]
			if !ok {
				hist = append(hist, 0)
				continue
			}
			hist = append(hist, v)
			seen++
			if v > 0 {
				valid++
				sum += v
			} else {
				fail++
			}
		}
		avg := 0.0
		if valid > 0 {
			avg = sum / float64(valid)
		}
		loss := 0.0
		if seen > 0 {
			loss = float64(fail) / float64(seen) * 100.0
		}
		current := 0.0
		for i := len(hist) - 1; i >= 0; i-- {
			if hist[i] != 0 {
				current = hist[i]
				break
			}
		}
		out = append(out, common.CardPingStatus{
			TargetName:   tgt,
			History60:    hist,
			AvgDelay1H:   avg,
			LossRate1H:   loss,
			CurrentDelay: current,
		})
	}
	return out
}

func main() {
	loadConfig()
	initDB()
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
	http.HandleFunc("/api/admin/config", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			configMutex.RLock()
			safeConfig := appConfig
			safeConfig.AdminPass = ""
			json.NewEncoder(w).Encode(safeConfig)
			configMutex.RUnlock()
		} else if r.Method == "POST" {
			var newConfig AppConfig
			if err := json.NewDecoder(r.Body).Decode(&newConfig); err == nil {
				configMutex.Lock()
				if newConfig.AdminPass == "" {
					newConfig.AdminPass = appConfig.AdminPass
				}
				appConfig = newConfig
				data, _ := json.MarshalIndent(appConfig, "", "  ")
				os.WriteFile("config.json", data, 0644)
				configMutex.Unlock()
				// 热下发新配置：不强制断开在线 Agent，直接推送最新指令
				configMutex.RLock()
				pTasks := appConfig.PingTasks
				nodeNameMap := map[string]string{}
				for _, n := range appConfig.Nodes {
					nodeNameMap[n.ID] = n.Name
				}
				configMutex.RUnlock()

				connMutex.Lock()
				for id, conn := range activeConns {
					name, ok := nodeNameMap[id]
					if !ok {
						conn.Close()
						delete(activeConns, id)
						mapMutex.Lock()
						st := serverStatusMap[id]
						st.IsOnline = false
						serverStatusMap[id] = st
						mapMutex.Unlock()
						continue
					}
					if err := conn.WriteJSON(common.AgentInstruction{ServerName: name, PingTasks: pTasks}); err != nil {
						conn.Close()
						delete(activeConns, id)
						mapMutex.Lock()
						st := serverStatusMap[id]
						st.IsOnline = false
						serverStatusMap[id] = st
						mapMutex.Unlock()
					}
				}
				connMutex.Unlock()
				w.Write([]byte(`{"status":"ok"}`))
			}
		}
	}))
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		configMutex.RLock()
		var cNode *common.NodeConfig
		for _, n := range appConfig.Nodes {
			if n.Token == token {
				cNode = &n
				break
			}
		}
		pTasks := appConfig.PingTasks
		configMutex.RUnlock()
		if cNode == nil {
			return
		}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		connMutex.Lock()
		activeConns[cNode.ID] = conn
		connMutex.Unlock()
		defer func() { connMutex.Lock(); delete(activeConns, cNode.ID); connMutex.Unlock() }()
		conn.WriteJSON(common.AgentInstruction{ServerName: cNode.Name, PingTasks: pTasks})
		mapMutex.Lock()
		st := serverStatusMap[cNode.ID]
		st.IsOnline = true
		serverStatusMap[cNode.ID] = st
		mapMutex.Unlock()
		for {
			if err := conn.ReadJSON(&st); err != nil {
				mapMutex.Lock()
				st = serverStatusMap[cNode.ID]
				st.IsOnline = false
				serverStatusMap[cNode.ID] = st
				mapMutex.Unlock()
				break
			}
			st.ServerID = cNode.ID
			st.IsOnline = true
			mapMutex.Lock()
			serverStatusMap[cNode.ID] = st
			mapMutex.Unlock()
			updateMonthlyUsage(cNode.ID, cNode.ExpireDate, st.NetInTransfer, st.NetOutTransfer)
		}
	})
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		configMutex.RLock()
		mapMutex.RLock()
		var nodes []FrontendNode
		for _, n := range appConfig.Nodes {
			st := serverStatusMap[n.ID]
			st.CardPingStatuses = buildCardPingStatuses(n.ID)
			nodes = append(nodes, buildFrontendNode(n, st))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"site_name": appConfig.SiteName, "nodes": nodes, "ping_tasks": appConfig.PingTasks})
		mapMutex.RUnlock()
		configMutex.RUnlock()
	})
	http.HandleFunc("/api/ping_history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		serverID := r.URL.Query().Get("server_id")
		hours := r.URL.Query().Get("hours")
		if hours == "" {
			hours = "24"
		}
		query := fmt.Sprintf(`SELECT datetime(timestamp, 'localtime'), target_name, delay, loss_rate FROM ping_history WHERE server_id = ? AND timestamp >= datetime('now', '-%s hours') ORDER BY timestamp ASC`, hours)
		rows, _ := db.Query(query, serverID)
		defer rows.Close()
		type DataPoint struct {
			Time   string  `json:"time"`
			Target string  `json:"target"`
			Delay  float64 `json:"delay"`
			Loss   float64 `json:"loss"`
		}
		points := make([]DataPoint, 0)
		for rows.Next() {
			var p DataPoint
			rows.Scan(&p.Time, &p.Target, &p.Delay, &p.Loss)
			points = append(points, p)
		}
		json.NewEncoder(w).Encode(points)
	})
	fmt.Println("🚀 服务端热刷新机制已激活！")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
