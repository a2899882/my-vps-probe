package main

import (
"encoding/json"
"fmt"
"log"
"net/http"
"sync"

"my-vps-probe/common"

"github.com/gorilla/websocket"
)

// serverStatusMap 在内存中存储所有小鸡的最新状态
// mapMutex 用于防止并发读写冲突
var (
serverStatusMap = make(map[string]common.ServerStatus)
mapMutex        sync.RWMutex
)

// 定义 WebSocket 升级器
var upgrader = websocket.Upgrader{
CheckOrigin: func(r *http.Request) bool {
return true // 允许跨域，方便前端联调
},
}

func main() {
// 1. 接收 Agent 数据的 WebSocket 路由
http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
// 极简安全校验：核对 Token (防止别人恶意上报数据)
token := r.URL.Query().Get("token")
if token != "my_secret_token_123" {
http.Error(w, "Unauthorized", http.StatusUnauthorized)
return
}

// 升级 HTTP 为 WebSocket 长连接
conn, err := upgrader.Upgrade(w, r, nil)
if err != nil {
log.Println("WebSocket 升级失败:", err)
return
}
defer conn.Close()

fmt.Println("🎉 一个 Agent 已成功连接:", r.RemoteAddr)

// 不断循环，接收 Agent 传来的 JSON 数据
for {
var status common.ServerStatus
err := conn.ReadJSON(&status)
if err != nil {
fmt.Println("Agent 断开连接:", err)
// 这里可以选择在 map 中将该服务器标记为离线，目前先直接跳出
break
}

// 将接收到的最新状态更新到内存中
mapMutex.Lock()
serverStatusMap[status.ServerID] = status
mapMutex.Unlock()
}
})

// 2. 提供给前端网页获取所有机器状态的 API 路由
http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
w.Header().Set("Access-Control-Allow-Origin", "*")

mapMutex.RLock()
json.NewEncoder(w).Encode(serverStatusMap)
mapMutex.RUnlock()
})

fmt.Println("🚀 主控服务端已启动，正在监听端口 :8080")
// 启动服务器
log.Fatal(http.ListenAndServe(":8080", nil))
}
