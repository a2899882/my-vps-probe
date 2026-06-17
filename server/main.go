package main

import (
"database/sql"
"encoding/json"
"fmt"
"log"
"net/http"
"os"
"sync"
"time"

"my-vps-probe/common"

"github.com/gorilla/websocket"
_ "modernc.org/sqlite"
)

var ( serverStatusMap = make(map[string]common.ServerStatus); appConfig common.AppConfig; configMutex sync.RWMutex; mapMutex sync.RWMutex; db *sql.DB )

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
return func(w http.ResponseWriter, r *http.Request) {
user, pass, ok := r.BasicAuth(); configMutex.RLock(); expectedUser := appConfig.AdminUser; expectedPass := appConfig.AdminPass; configMutex.RUnlock()
if !ok || user != expectedUser || pass != expectedPass { w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`); http.Error(w, "Unauthorized", http.StatusUnauthorized); return }
next(w, r)
}
}

func initDB() {
db, _ = sql.Open("sqlite", "data.db"); db.Exec(`CREATE TABLE IF NOT EXISTS ping_history (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP, server_id TEXT, target_name TEXT, delay REAL, loss_rate REAL);`)
go func() { for { time.Sleep(1 * time.Minute); saveHistoryToDB(); db.Exec("DELETE FROM ping_history WHERE timestamp <= datetime('now', '-3 days')") } }()
}

func saveHistoryToDB() {
mapMutex.RLock(); defer mapMutex.RUnlock(); tx, _ := db.Begin(); defer tx.Commit(); stmt, _ := tx.Prepare("INSERT INTO ping_history (server_id, target_name, delay, loss_rate) VALUES (?, ?, ?, ?)"); defer stmt.Close()
for serverID, status := range serverStatusMap { if !status.IsOnline { continue }; for _, ping := range status.PingStatuses { stmt.Exec(serverID, ping.TargetName, ping.CurrentDelay, ping.LossRate) } }
}

func loadConfig() {
data, err := os.ReadFile("config.json")
if err == nil { json.Unmarshal(data, &appConfig) } else {
appConfig = common.AppConfig{ SiteName: "极简探针看板", AdminUser: "admin", AdminPass: "123456", Nodes: []common.NodeConfig{ {ID: "node-1", Name: "主控测试机", Token: "my_secret_token_123", ExpireDate: "2027/05/13", Region: "US"} }, PingTasks: []common.PingTask{ {Name: "Cloudflare", Host: "1.1.1.1"}, {Name: "Google", Host: "8.8.8.8"} } }
data, _ := json.MarshalIndent(appConfig, "", "  "); os.WriteFile("config.json", data, 0644)
}
if appConfig.AdminUser == "" { appConfig.AdminUser = "admin" }; if appConfig.AdminPass == "" { appConfig.AdminPass = "123456" }; if appConfig.SiteName == "" { appConfig.SiteName = "极简探针看板" }
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
type FrontendNode struct { common.NodeConfig; Status common.ServerStatus `json:"status"` }

func main() {
loadConfig(); initDB(); defer db.Close()
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "server/index.html") })
http.HandleFunc("/admin", basicAuth(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "server/admin.html") }))
http.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "install.sh") })
http.HandleFunc("/download/agent.go", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "agent/main.go") })

http.HandleFunc("/api/admin/config", basicAuth(func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
if r.Method == "GET" { 
configMutex.RLock(); safeConfig := appConfig; safeConfig.AdminPass = ""; json.NewEncoder(w).Encode(safeConfig); configMutex.RUnlock() 
} else if r.Method == "POST" { 
var newConfig common.AppConfig; if err := json.NewDecoder(r.Body).Decode(&newConfig); err == nil { configMutex.Lock(); if newConfig.AdminPass == "" { newConfig.AdminPass = appConfig.AdminPass }; appConfig = newConfig; data, _ := json.MarshalIndent(appConfig, "", "  "); os.WriteFile("config.json", data, 0644); configMutex.Unlock(); w.Write([]byte(`{"status":"ok"}`)) } 
}
}))

http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
token := r.URL.Query().Get("token"); configMutex.RLock(); var cNode *common.NodeConfig
for _, n := range appConfig.Nodes { if n.Token == token { cNode = &n; break } }
pTasks := appConfig.PingTasks; configMutex.RUnlock(); if cNode == nil { return }
conn, _ := upgrader.Upgrade(w, r, nil); defer conn.Close()
conn.WriteJSON(common.AgentInstruction{ ServerName: cNode.Name, PingTasks: pTasks })
mapMutex.Lock(); st := serverStatusMap[cNode.ID]; st.IsOnline = true; serverStatusMap[cNode.ID] = st; mapMutex.Unlock()
for { if err := conn.ReadJSON(&st); err != nil { mapMutex.Lock(); st = serverStatusMap[cNode.ID]; st.IsOnline = false; serverStatusMap[cNode.ID] = st; mapMutex.Unlock(); break }; st.ServerID = cNode.ID; st.IsOnline = true; mapMutex.Lock(); serverStatusMap[cNode.ID] = st; mapMutex.Unlock() }
})

http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json"); configMutex.RLock(); mapMutex.RLock(); var nodes []FrontendNode
for _, n := range appConfig.Nodes { nodes = append(nodes, FrontendNode{ NodeConfig: n, Status: serverStatusMap[n.ID] }) }
response := map[string]interface{}{ "site_name": appConfig.SiteName, "nodes": nodes }
mapMutex.RUnlock(); configMutex.RUnlock(); json.NewEncoder(w).Encode(response)
})

http.HandleFunc("/api/ping_history", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json"); serverID := r.URL.Query().Get("server_id"); hours := r.URL.Query().Get("hours"); if hours == "" { hours = "24" }
query := fmt.Sprintf(`SELECT datetime(timestamp, 'localtime'), target_name, delay, loss_rate FROM ping_history WHERE server_id = ? AND timestamp >= datetime('now', '-%s hours') ORDER BY timestamp ASC`, hours)
rows, _ := db.Query(query, serverID); defer rows.Close(); type DataPoint struct { Time string `json:"time"`; Target string `json:"target"`; Delay float64 `json:"delay"`; Loss float64 `json:"loss"` }; var points []DataPoint
for rows.Next() { var p DataPoint; rows.Scan(&p.Time, &p.Target, &p.Delay, &p.Loss); points = append(points, p) }; json.NewEncoder(w).Encode(points)
})

fmt.Println("🚀 服务端启动！已开启完整看板、动态数据统计与旗帜支持！")
log.Fatal(http.ListenAndServe(":8080", nil))
}
