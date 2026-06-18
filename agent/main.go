package main
import ("encoding/json"; "flag"; "fmt"; "net"; "net/http"; "os/exec"; "strings"; "time"; "github.com/gorilla/websocket"; "github.com/shirou/gopsutil/v3/cpu"; "github.com/shirou/gopsutil/v3/disk"; "github.com/shirou/gopsutil/v3/host"; "github.com/shirou/gopsutil/v3/load"; "github.com/shirou/gopsutil/v3/mem"; psnet "github.com/shirou/gopsutil/v3/net")
type PingTask struct { Name string `json:"name"`; Host string `json:"host"`; Type string `json:"type"` }
type AgentInstruction struct { ServerName string `json:"server_name"`; PingTasks []PingTask `json:"ping_tasks"` }
type PingResult struct { TargetName string `json:"target_name"`; CurrentDelay float64 `json:"current_delay"`; AvgDelay float64 `json:"avg_delay"`; LossRate float64 `json:"loss_rate"`; History []int `json:"history"` }
type ServerStatus struct { ServerID string `json:"server_id"`; IsOnline bool `json:"is_online"`; Uptime uint64 `json:"uptime"`; Load1 float64 `json:"load_1"`; CPUCores int `json:"cpu_cores"`; SwapTotal uint64 `json:"swap_total"`; SwapUsed uint64 `json:"swap_used"`; CPUUsage float64 `json:"cpu_usage"`; MemTotal uint64 `json:"mem_total"`; MemUsed uint64 `json:"mem_used"`; DiskTotal uint64 `json:"disk_total"`; DiskUsed uint64 `json:"disk_used"`; NetInSpeed uint64 `json:"net_in_speed"`; NetOutSpeed uint64 `json:"net_out_speed"`; NetInTransfer uint64 `json:"net_in_transfer"`; NetOutTransfer uint64 `json:"net_out_transfer"`; CountryCode string `json:"country_code"`; PingStatuses []PingResult `json:"ping_statuses"` }

type PingTracker struct { History []int; HourDelays []float64; TickSum float64; TickCount int; TickFails int; LastEma float64 }
var trackers = make(map[string]*PingTracker); var tickCount = 0; var serverAddr, token string; var globalCountryCode = "OT"; var lastNetBytesRecv, lastNetBytesSent uint64
func init() { go func() { resp, err := http.Get("http://ip-api.com/json/"); if err == nil { defer resp.Body.Close(); var res struct { CountryCode string `json:"countryCode"` }; json.NewDecoder(resp.Body).Decode(&res); if res.CountryCode != "" { globalCountryCode = res.CountryCode } } }() }
func main() { flag.StringVar(&serverAddr, "server", "localhost:8080", "主控地址"); flag.StringVar(&token, "token", "123", "Token"); flag.Parse(); for { connectAndReport(); time.Sleep(5 * time.Second) } }

func connectAndReport() { cleanAddr := strings.TrimPrefix(strings.TrimPrefix(serverAddr, "http://"), "https://"); wsScheme := "ws://"; if strings.HasPrefix(serverAddr, "https://") || strings.HasSuffix(serverAddr, "443") { wsScheme = "wss://" }; conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s%s/ws?token=%s", wsScheme, cleanAddr, token), nil); if err != nil { return }; defer conn.Close(); var instr AgentInstruction; if err := conn.ReadJSON(&instr); err != nil { return }
for { 
status := ServerStatus{IsOnline: true, CountryCode: globalCountryCode}; if h, err := host.Info(); err == nil && h != nil { status.Uptime = h.Uptime }; if l, err := load.Avg(); err == nil && l != nil { status.Load1 = l.Load1 }; if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 { status.CPUUsage = c[0] }; if cores, err := cpu.Counts(true); err == nil { status.CPUCores = cores }; if v, err := mem.VirtualMemory(); err == nil && v != nil { status.MemTotal = v.Total; status.MemUsed = v.Used }; if s, err := mem.SwapMemory(); err == nil && s != nil { status.SwapTotal = s.Total; status.SwapUsed = s.Used }; if d, err := disk.Usage("/"); err == nil && d != nil { status.DiskTotal = d.Total; status.DiskUsed = d.Used }; if n, err := psnet.IOCounters(false); err == nil && len(n) > 0 { status.NetInTransfer = n[0].BytesRecv; status.NetOutTransfer = n[0].BytesSent; if lastNetBytesRecv > 0 { status.NetInSpeed = (n[0].BytesRecv - lastNetBytesRecv) / 2; status.NetOutSpeed = (n[0].BytesSent - lastNetBytesSent) / 2 }; lastNetBytesRecv = n[0].BytesRecv; lastNetBytesSent = n[0].BytesSent }

newTrackers := make(map[string]*PingTracker); for _, task := range instr.PingTasks { if val, ok := trackers[task.Name]; ok { newTrackers[task.Name] = val } else { newTrackers[task.Name] = &PingTracker{History: make([]int, 0), HourDelays: make([]float64, 0)} } }; trackers = newTrackers
var pingResults []PingResult
tickCount++
isMinuteTick := (tickCount % 30 == 0)

for _, task := range instr.PingTasks { 
delay, success := performPing(task); t := trackers[task.Name]
if success { if t.LastEma == 0 { t.LastEma = delay } else { t.LastEma = t.LastEma * 0.8 + delay * 0.2 } }
if success { t.TickSum += delay; t.TickCount++ } else { t.TickFails++ }
if isMinuteTick {
if t.TickCount > 0 { t.History = append(t.History, 1); t.HourDelays = append(t.HourDelays, t.TickSum / float64(t.TickCount)) } else { t.History = append(t.History, 0) }
if len(t.History) > 60 { t.History = t.History[1:] }
if len(t.HourDelays) > 60 { t.HourDelays = t.HourDelays[1:] }
t.TickSum = 0; t.TickCount = 0; t.TickFails = 0
}
var hourAvg float64 = 0
if len(t.HourDelays) > 0 { var sum float64 = 0; for _, v := range t.HourDelays { sum += v }; hourAvg = sum / float64(len(t.HourDelays)) }
fails := 0; for _, v := range t.History { if v == 0 { fails++ } }
loss := 0.0; if len(t.History) > 0 { loss = float64(fails) / float64(len(t.History)) * 100.0 }
pingResults = append(pingResults, PingResult{TargetName: task.Name, CurrentDelay: t.LastEma, AvgDelay: hourAvg, LossRate: loss, History: t.History})
}
status.PingStatuses = pingResults; if err := conn.WriteJSON(status); err != nil { return }; time.Sleep(2 * time.Second)
}
}
func performPing(task PingTask) (float64, bool) { 
if task.Type == "ICMP" { cmd := exec.Command("ping", "-c", "1", "-W", "1", task.Host); out, err := cmd.Output(); if err != nil { return 0, false }; idx := strings.Index(string(out), "time="); if idx != -1 { endIdx := strings.Index(string(out)[idx:], " ms"); if endIdx != -1 { var delay float64; fmt.Sscanf(string(out)[idx+5:idx+endIdx], "%f", &delay); return delay, true } }; return 1.0, true }
host := task.Host; if !strings.Contains(host, ":") { host = host + ":80" }; start := time.Now(); conn, err := net.DialTimeout("tcp", host, 2*time.Second); if err != nil { return 0, false }; conn.Close(); return float64(time.Since(start).Milliseconds()), true 
}
