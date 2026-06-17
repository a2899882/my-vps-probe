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
"github.com/shirou/gopsutil/v3/host"
"github.com/shirou/gopsutil/v3/load"
"github.com/shirou/gopsutil/v3/mem"
"github.com/shirou/gopsutil/v3/net"
)

var lastNetBytesRecv, lastNetBytesSent uint64

// 用于 1 小时 60 格子的状态追踪
type PingTracker struct {
History      []int // 保存 60 个格子的状态 (0 或 1)
FailsThisMin int   // 这一分钟内丢包的次数
LastDelay    float64
}
var trackers = make(map[string]*PingTracker)
var tickCount = 0

const serverWSURL = "ws://localhost:8080/ws?token=my_secret_token_123"

func main() { for { connectAndReport(); time.Sleep(5 * time.Second) } }

func connectAndReport() {
conn, _, err := websocket.DefaultDialer.Dial(serverWSURL, nil)
if err != nil { log.Println("连接失败:", err); return }
defer conn.Close()

var instruction common.AgentInstruction
if err := conn.ReadJSON(&instruction); err != nil { return }

for {
status := collectData(instruction.PingTasks)
if err := conn.WriteJSON(status); err != nil { return }
time.Sleep(2 * time.Second)
}
}

func collectData(tasks []common.PingTask) common.ServerStatus {
var status common.ServerStatus
hInfo, _ := host.Info(); if hInfo != nil { status.Uptime = hInfo.Uptime }
lInfo, _ := load.Avg(); if lInfo != nil { status.Load1 = lInfo.Load1 }
cpuPercent, _ := cpu.Percent(0, false); if len(cpuPercent) > 0 { status.CPUUsage = cpuPercent[0] }
cores, _ := cpu.Counts(true); status.CPUCores = cores
vMem, _ := mem.VirtualMemory(); if vMem != nil { status.MemTotal = vMem.Total; status.MemUsed = vMem.Used }
sMem, _ := mem.SwapMemory(); if sMem != nil { status.SwapTotal = sMem.Total; status.SwapUsed = sMem.Used }
dInfo, _ := disk.Usage("/"); if dInfo != nil { status.DiskTotal = dInfo.Total; status.DiskUsed = dInfo.Used }
nInfo, _ := net.IOCounters(false)
if len(nInfo) > 0 {
status.NetInTransfer = nInfo[0].BytesRecv; status.NetOutTransfer = nInfo[0].BytesSent
if lastNetBytesRecv > 0 { status.NetInSpeed = (nInfo[0].BytesRecv - lastNetBytesRecv) / 2; status.NetOutSpeed = (nInfo[0].BytesSent - lastNetBytesSent) / 2 }
lastNetBytesRecv = nInfo[0].BytesRecv; lastNetBytesSent = nInfo[0].BytesSent
}

tickCount++
isMinuteTick := (tickCount % 30 == 0) // 每执行 30 次 (60秒) 结算一次分钟状态

var pingResults []common.PingResult
for _, task := range tasks {
delay, success := doPing(task.Host)
t, ok := trackers[task.Name]
if !ok { t = &PingTracker{History: make([]int, 0)}; trackers[task.Name] = t }

t.LastDelay = delay
if !success { t.FailsThisMin++ }

// 每满一分钟，生成一个代表这一分钟的红绿块 (60块=1小时)
if isMinuteTick {
if t.FailsThisMin > 0 { t.History = append(t.History, 0) } else { t.History = append(t.History, 1) }
if len(t.History) > 60 { t.History = t.History[1:] } // 保持最多 60 个块
t.FailsThisMin = 0 // 重置计数器
}

failCount := 0; for _, v := range t.History { if v == 0 { failCount++ } }
lossRate := 0.0; if len(t.History) > 0 { lossRate = float64(failCount) / float64(len(t.History)) * 100 }

pingResults = append(pingResults, common.PingResult{ TargetName: task.Name, CurrentDelay: delay, LossRate: lossRate, History: t.History })
}
status.PingStatuses = pingResults
return status
}

func doPing(ip string) (float64, bool) {
cmd := exec.Command("ping", "-c", "1", "-W", "1", ip)
out, err := cmd.Output(); if err != nil { return 0, false }
idx := strings.Index(string(out), "time=")
if idx != -1 {
endIdx := strings.Index(string(out)[idx:], " ms")
if endIdx != -1 { var delay float64; fmt.Sscanf(string(out)[idx+5:idx+endIdx], "%f", &delay); return delay, true }
}
return 0, true
}
