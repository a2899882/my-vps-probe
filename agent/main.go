package main

import (
"encoding/json"
"flag"
"fmt"
"net"
"net/http"
"strings"
"time"

"github.com/gorilla/websocket"
"github.com/shirou/gopsutil/v3/cpu"
"github.com/shirou/gopsutil/v3/disk"
"github.com/shirou/gopsutil/v3/host"
"github.com/shirou/gopsutil/v3/load"
"github.com/shirou/gopsutil/v3/mem"
psnet "github.com/shirou/gopsutil/v3/net"
"my-vps-probe/common"
)

type PingTracker struct {
History   []float64
TickSum   float64
TickCount int
TickFails int
LastDelay float64
}

var (
trackers                    = make(map[string]*PingTracker)
tickCount                   = 0
serverAddr, token           string
globalCountryCode           = "OT"
lastNetBytesRecv, lastNetBytesSent uint64
)

func init() {
go func() {
resp, err := http.Get("http://ip-api.com/json/")
if err == nil {
defer resp.Body.Close()
var res struct {
CountryCode string `json:"countryCode"`
}
json.NewDecoder(resp.Body).Decode(&res)
if res.CountryCode != "" {
globalCountryCode = res.CountryCode
}
}
}()
}

func main() {
flag.StringVar(&serverAddr, "server", "localhost:8080", "主控地址")
flag.StringVar(&token, "token", "123", "Token")
flag.Parse()
for {
connectAndReport()
time.Sleep(5 * time.Second)
}
}

func connectAndReport() {
cleanAddr := strings.TrimPrefix(strings.TrimPrefix(serverAddr, "http://"), "https://")
wsScheme := "ws://"
if strings.HasPrefix(serverAddr, "https://") || strings.HasSuffix(serverAddr, ":443") {
wsScheme = "wss://"
}
conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s%s/ws?token=%s", wsScheme, cleanAddr, token), nil)
if err != nil {
return
}
defer conn.Close()

var instr common.AgentInstruction
if err := conn.ReadJSON(&instr); err != nil {
return
}

for {
status := common.ServerStatus{IsOnline: true, CountryCode: globalCountryCode}
if h, err := host.Info(); err == nil && h != nil {
status.Uptime = h.Uptime
}
if l, err := load.Avg(); err == nil && l != nil {
status.Load1 = l.Load1
}
if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 {
status.CPUUsage = c[0]
}
if cores, err := cpu.Counts(true); err == nil {
status.CPUCores = cores
}
if v, err := mem.VirtualMemory(); err == nil && v != nil {
status.MemTotal = v.Total
status.MemUsed = v.Used
}
if s, err := mem.SwapMemory(); err == nil && s != nil {
status.SwapTotal = s.Total
status.SwapUsed = s.Used
}
if d, err := disk.Usage("/"); err == nil && d != nil {
status.DiskTotal = d.Total
status.DiskUsed = d.Used
}
if n, err := psnet.IOCounters(false); err == nil && len(n) > 0 {
status.NetInTransfer = n[0].BytesRecv
status.NetOutTransfer = n[0].BytesSent
if lastNetBytesRecv > 0 {
status.NetInSpeed = (n[0].BytesRecv - lastNetBytesRecv) / 2
status.NetOutSpeed = (n[0].BytesSent - lastNetBytesSent) / 2
}
lastNetBytesRecv = n[0].BytesRecv
lastNetBytesSent = n[0].BytesSent
}

newTrackers := make(map[string]*PingTracker)
for _, task := range instr.PingTasks {
if val, ok := trackers[task.Name]; ok {
newTrackers[task.Name] = val
} else {
newTrackers[task.Name] = &PingTracker{History: make([]float64, 0)}
}
}
trackers = newTrackers

tickCount++
isMinuteTick := (tickCount % 30 == 0)

var pingResults []common.PingResult
for _, task := range instr.PingTasks {
delay, success := tcpPing(task.Host)
t := trackers[task.Name]

if success {
t.LastDelay = delay
t.TickSum += delay
t.TickCount++
} else {
t.LastDelay = 0
t.TickFails++
}

if isMinuteTick {
if t.TickCount > 0 {
t.History = append(t.History, t.TickSum/float64(t.TickCount))
} else {
t.History = append(t.History, 0)
}
if len(t.History) > 60 {
t.History = t.History[1:]
}
t.TickSum = 0
t.TickCount = 0
t.TickFails = 0
}

fails := 0
for _, v := range t.History {
if v == 0 {
fails++
}
}
loss := 0.0
if len(t.History) > 0 {
loss = float64(fails) / float64(len(t.History)) * 100.0
}

var avgDelay float64
validCount := 0
for _, v := range t.History {
if v > 0 {
avgDelay += v
validCount++
}
}
if validCount > 0 {
avgDelay /= float64(validCount)
}

pingResults = append(pingResults, common.PingResult{
TargetName:   task.Name,
CurrentDelay: t.LastDelay,
AvgDelay:     avgDelay,
LossRate:     loss,
History:      t.History,
})
}

status.PingStatuses = pingResults
if err := conn.WriteJSON(status); err != nil {
return
}
time.Sleep(2 * time.Second)
}
}

func tcpPing(host string) (float64, bool) {
addr := host
if !strings.Contains(addr, ":") {
addr = addr + ":80"
}
start := time.Now()
conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
if err != nil {
return 0, false
}
conn.Close()
return float64(time.Since(start).Milliseconds()), true
}
