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

var (
serverStatusMap = make(map[string]common.ServerStatus)
appConfig       common.AppConfig
configMutex     sync.RWMutex
mapMutex        sync.RWMutex
db              *sql.DB
)

func initDB() {
var err error
db, err = sql.Open("sqlite", "data.db")
if err != nil { log.Fatal(err) }

db.Exec(`CREATE TABLE IF NOT EXISTS ping_history (
id INTEGER PRIMARY KEY AUTOINCREMENT,
timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
server_id TEXT,
target_name TEXT,
delay REAL,
loss_rate REAL
);`)

go func() {
for {
time.Sleep(1 * time.Minute)
saveHistoryToDB()
db.Exec("DELETE FROM ping_history WHERE timestamp <= datetime('now', '-3 days')")
}
}()
}

func saveHistoryToDB() {
mapMutex.RLock(); defer mapMutex.RUnlock()
tx, _ := db.Begin(); defer tx.Commit()
stmt, _ := tx.Prepare("INSERT INTO ping_history (server_id, target_name, delay, loss_rate) VALUES (?, ?, ?, ?)")
defer stmt.Close()

for serverID, status := range serverStatusMap {
if !status.IsOnline { continue }
for _, ping := range status.PingStatuses {
stmt.Exec(serverID, ping.TargetName, ping.CurrentDelay, ping.LossRate)
}
}
}

func loadConfig() {
data, err := os.ReadFile("config.json")
if err == nil { json.Unmarshal(data, &appConfig) } else {
appConfig = common.AppConfig{
Nodes: []common.NodeConfig{ {ID: "node-1", Name: "主控测试机", Token: "my_secret_token_123", ExpireDate: "2027/05/13"} },
PingTasks: []common.PingTask{ {Name: "广东移动", Host: "120.196.165.24"}, {Name: "广东电信", Host: "14.215.177.39"} },
}
data, _ := json.MarshalIndent(appConfig, "", "  ")
os.WriteFile("config.json", data, 0644)
}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
type FrontendNode struct { common.NodeConfig; Status common.ServerStatus `json:"status"` }

func main() {
loadConfig(); initDB(); defer db.Close()
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "server/index.html") })
http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
token := r.URL.Query().Get("token")
configMutex.RLock(); var cNode *common.NodeConfig
for _, n := range appConfig.Nodes { if n.Token == token { cNode = &n; break } }
pTasks := appConfig.PingTasks; configMutex.RUnlock()
if cNode == nil { return }
conn, _ := upgrader.Upgrade(w, r, nil); defer conn.Close()
conn.WriteJSON(common.AgentInstruction{ ServerName: cNode.Name, PingTasks: pTasks })

mapMutex.Lock(); st := serverStatusMap[cNode.ID]; st.IsOnline = true; serverStatusMap[cNode.ID] = st; mapMutex.Unlock()
for {
if err := conn.ReadJSON(&st); err != nil {
mapMutex.Lock(); st = serverStatusMap[cNode.ID]; st.IsOnline = false; serverStatusMap[cNode.ID] = st; mapMutex.Unlock()
break
}
st.ServerID = cNode.ID; st.IsOnline = true
mapMutex.Lock(); serverStatusMap[cNode.ID] = st; mapMutex.Unlock()
}
})

http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json"); configMutex.RLock(); mapMutex.RLock()
var result []FrontendNode
for _, n := range appConfig.Nodes { result = append(result, FrontendNode{ NodeConfig: n, Status: serverStatusMap[n.ID] }) }
mapMutex.RUnlock(); configMutex.RUnlock(); json.NewEncoder(w).Encode(result)
})

// 【修改】获取该节点下的 所有 目标历史数据，实现聚合折线图
http.HandleFunc("/api/ping_history", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
serverID := r.URL.Query().Get("server_id")
hours := r.URL.Query().Get("hours"); if hours == "" { hours = "24" }

query := fmt.Sprintf(`SELECT datetime(timestamp, 'localtime'), target_name, delay, loss_rate 
FROM ping_history WHERE server_id = ? AND timestamp >= datetime('now', '-%s hours') ORDER BY timestamp ASC`, hours)
rows, _ := db.Query(query, serverID); defer rows.Close()

type DataPoint struct { Time string `json:"time"`; Target string `json:"target"`; Delay float64 `json:"delay"`; Loss float64 `json:"loss"` }
var points []DataPoint
for rows.Next() { var p DataPoint; rows.Scan(&p.Time, &p.Target, &p.Delay, &p.Loss); points = append(points, p) }
json.NewEncoder(w).Encode(points)
})

fmt.Println("🚀 服务端聚合 API 升级完毕！")
log.Fatal(http.ListenAndServe(":8080", nil))
}
