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
History []float64
Last    float64
}

var (
trackers                           = make(map[string]*PingTracker)
serverAddr, token                  string
globalCountryCode                  = "OT"
lastNetBytesRecv, lastNetBytesSent uint64
lastNetAt time.Time
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
if h, err := host.Info(); err == nil && h != nil { status.Uptime = h.Uptime }
if l, err := load.Avg(); err == nil && l != nil { status.Load1 = l.Load1 }
if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 { status.CPUUsage = c[0] }
if cores, err := cpu.Counts(true); err == nil { status.CPUCores = cores }
if v, err := mem.VirtualMemory(); err == nil && v != nil { status.MemTotal = v.Total; status.MemUsed = v.Used }
if s, err := mem.SwapMemory(); err == nil && s != nil { status.SwapTotal = s.Total; status.SwapUsed = s.Used }
if d, err := disk.Usage("/"); err == nil && d != nil { status.DiskTotal = d.Total; status.DiskUsed = d.Used }
if n, err := psnet.IOCounters(false); err == nil && len(n) > 0 {
status.NetInTransfer = n[0].BytesRecv
status.NetOutTransfer = n[0].BytesSent
now := time.Now()
if lastNetBytesRecv > 0 && !lastNetAt.IsZero() {
secs := now.Sub(lastNetAt).Seconds()
if secs > 0 {
status.NetInSpeed = uint64(float64(n[0].BytesRecv-lastNetBytesRecv) / secs)
status.NetOutSpeed = uint64(float64(n[0].BytesSent-lastNetBytesSent) / secs)
}
}
lastNetBytesRecv = n[0].BytesRecv
lastNetBytesSent = n[0].BytesSent
lastNetAt = now
}

newTrackers := make(map[string]*PingTracker)
for _, task := range instr.PingTasks {
if val, ok := trackers[task.Name]; ok {
newTrackers[task.Name] = val
} else {
newTrackers[task.Name] = &PingTracker{History: make([]float64, 0), Last: -1}
}
}
trackers = newTrackers

var pingResults []common.PingResult
for _, task := range instr.PingTasks {
delay, success := tcpPing(task.Host)
t := trackers[task.Name]

if success {
t.Last = delay
t.History = append(t.History, delay)
} else {
t.Last = -1
t.History = append(t.History, -1)
}
if len(t.History) > 60 {
t.History = t.History[len(t.History)-60:]
}

var sum float64
valid := 0
fail := 0
for _, v := range t.History {
if v > 0 {
sum += v
valid++
} else {
fail++
}
}
avg := 0.0
if valid > 0 {
avg = sum / float64(valid)
}
loss := 0.0
if len(t.History) > 0 {
loss = float64(fail) / float64(len(t.History)) * 100.0
}

pingResults = append(pingResults, common.PingResult{
TargetName:   task.Name,
CurrentDelay: t.Last,
AvgDelay:     avg,
LossRate:     loss,
History:      append([]float64(nil), t.History...),
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
addr += ":80"
}
start := time.Now()
conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
if err != nil {
return 0, false
}
conn.Close()
return float64(time.Since(start).Milliseconds()), true
}
