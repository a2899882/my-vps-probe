package main

import (
"encoding/json"
"fmt"
"os/exec"
"strings"
"time"

"my-vps-probe/common"

"github.com/shirou/gopsutil/v3/cpu"
"github.com/shirou/gopsutil/v3/disk"
"github.com/shirou/gopsutil/v3/mem"
"github.com/shirou/gopsutil/v3/net"
)

// 维护上一秒的网络数据，用于计算实时网速
var lastNetBytesRecv uint64
var lastNetBytesSent uint64

// 维护多点 Ping 的历史状态 (0代表丢包, 1代表通畅)
var pingHistories = make(map[string][]int)

func main() {
fmt.Println("🚀 Agent 启动成功，正在采集系统状态与网络延迟...")

// 预设两个测试目标：Cloudflare 和 Google DNS（后期可由主控端下发）
targets := []string{"1.1.1.1", "8.8.8.8"}

for {
status := collectData(targets)

// 将采集到的结构体转换为漂亮的 JSON 格式打印出来
jsonData, _ := json.MarshalIndent(status, "", "  ")
fmt.Println(string(jsonData))

time.Sleep(2 * time.Second) // 每 2 秒采集一次
}
}

func collectData(targets []string) common.ServerStatus {
var status common.ServerStatus
status.ServerID = "my-debian-node"

// 1. CPU 负载
cpuPercent, _ := cpu.Percent(0, false)
if len(cpuPercent) > 0 {
status.CPUUsage = cpuPercent[0]
}

// 2. 内存使用
vMem, _ := mem.VirtualMemory()
if vMem != nil {
status.MemTotal = vMem.Total
status.MemUsed = vMem.Used
}

// 3. 硬盘使用 (主目录)
dInfo, _ := disk.Usage("/")
if dInfo != nil {
status.DiskTotal = dInfo.Total
status.DiskUsed = dInfo.Used
}

// 4. 网络流量与实时网速
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

// 5. 执行 Ping 并维护前端需要的“红绿条”历史数组
var pingResults []common.PingResult
for _, ip := range targets {
delay, success := doPing(ip)

// 获取该 IP 的历史记录
hist := pingHistories[ip]
if success {
hist = append(hist, 1) // 成功记录 1
} else {
hist = append(hist, 0) // 丢包记录 0
}
// 保持数组长度为 30（对应前端的 30 个像素块）
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

// 调用 Debian 系统的 ping 命令，解析延迟数据
func doPing(ip string) (float64, bool) {
// 发送 1 个包，超时时间 1 秒
cmd := exec.Command("ping", "-c", "1", "-W", "1", ip)
out, err := cmd.Output()
if err != nil {
return 0, false // 命令报错即视为丢包
}

// 从输出中提取 "time=XX.X ms" 的数值
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

// 计算丢包率
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
