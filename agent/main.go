package main

import (
"fmt"
"log"
"os/exec"
"strings"
"time"

"my-vps-probe/common"

"github.com/gorilla/websocket"
"github.com/shirou/gopsutil/v3/cpu"
"github.com/shirou/gopsutil/v3/disk"
"github.com/shirou/gopsutil/v3/mem"
"github.com/shirou/gopsutil/v3/net"
)

var lastNetBytesRecv uint64
var lastNetBytesSent uint64
var pingHistories = make(map[string][]int)

// 填写主控服务端的 WebSocket 地址和安全 Token (如果在一台机测试，写 localhost 即可)
const serverWSURL = "ws://localhost:8080/ws?token=my_secret_token_123"

func main() {
fmt.Println("🚀 Agent 启动成功，准备连接服务端...")
targets := []string{"1.1.1.1", "8.8.8.8"}

// 外层死循环：保证断线后能无限次重连
for {
connectAndReport(targets)
fmt.Println("⚠️ 与服务端的连接断开，5秒后尝试重连...")
time.Sleep(5 * time.Second)
}
}

// 核心长连接发送逻辑
func connectAndReport(targets []string) {
dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
conn, _, err := dialer.Dial(serverWSURL, nil)
if err != nil {
log.Println("连接服务端失败:", err)
return
}
defer conn.Close()
fmt.Println("✅ 成功连接到主控服务端！开始每2秒上报一次实时数据...")

for {
status := collectData(targets)
err := conn.WriteJSON(status)
if err != nil {
log.Println("发送数据失败，网络异常:", err)
return // 退出当前循环，触发外层的5秒重连
}
time.Sleep(2 * time.Second)
}
}

// ================= 以下为原版数据采集逻辑，保持不变 =================

func collectData(targets []string) common.ServerStatus {
var status common.ServerStatus
status.ServerID = "my-debian-node"

cpuPercent, _ := cpu.Percent(0, false)
if len(cpuPercent) > 0 {
status.CPUUsage = cpuPercent[0]
}

vMem, _ := mem.VirtualMemory()
if vMem != nil {
status.MemTotal = vMem.Total
status.MemUsed = vMem.Used
}

dInfo, _ := disk.Usage("/")
if dInfo != nil {
status.DiskTotal = dInfo.Total
status.DiskUsed = dInfo.Used
}

nInfo, _ := net.IOCounters(false)
if len(nInfo) > 0 {
status.NetInTransfer = nInfo[0].BytesRecv
status.NetOutTransfer = nInfo[0].BytesSent
if lastNetBytesRecv > 0 {
status.NetInSpeed = (nInfo[0].BytesRecv - lastNetBytesRecv) / 2
status.NetOutSpeed = (nInfo[0].BytesSent - lastNetBytesSent) / 2
}
lastNetBytesRecv = nInfo[0].BytesRecv
lastNetBytesSent = nInfo[0].BytesSent
}

var pingResults []common.PingResult
for _, ip := range targets {
delay, success := doPing(ip)
hist := pingHistories[ip]
if success {
hist = append(hist, 1)
} else {
hist = append(hist, 0)
}
if len(hist) > 30 {
hist = hist[1:]
}
pingHistories[ip] = hist

pingResults = append(pingResults, common.PingResult{
TargetName:   ip,
CurrentDelay: delay,
LossRate:     calculateLossRate(hist),
History:      hist,
})
}
status.PingStatuses = pingResults
return status
}

func doPing(ip string) (float64, bool) {
cmd := exec.Command("ping", "-c", "1", "-W", "1", ip)
out, err := cmd.Output()
if err != nil {
return 0, false
}
outStr := string(out)
idx := strings.Index(outStr, "time=")
if idx != -1 {
endIdx := strings.Index(outStr[idx:], " ms")
if endIdx != -1 {
timeStr := outStr[idx+5 : idx+endIdx]
var delay float64
fmt.Sscanf(timeStr, "%f", &delay)
return delay, true
}
}
return 0, true
}

func calculateLossRate(hist []int) float64 {
if len(hist) == 0 {
return 0
}
failCount := 0
for _, v := range hist {
if v == 0 {
failCount++
}
}
return float64(failCount) / float64(len(hist)) * 100
}
