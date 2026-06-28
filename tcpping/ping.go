package ping

import (
"net"
"strings"
"time"
)

type Statistics struct {
PacketsRecv int
PacketsSent int
PacketLoss  float64
Addr        string
AvgRtt      time.Duration
}

type Pinger struct {
Count    int
Timeout  time.Duration
Interval time.Duration
OnRecv   func(interface{})
OnFinish func(*Statistics)
stats    Statistics
target   string
}

func NewPinger(addr string) (*Pinger, error) {
return &Pinger{
target:   addr,
Count:    3,
Timeout:  time.Second * 2,
Interval: time.Millisecond * 500,
}, nil
}

func (p *Pinger) SetPrivileged(b bool) {}

func (p *Pinger) Run() error {
p.stats.PacketsSent = p.Count
success := 0
var total time.Duration

addr := p.target
if !strings.Contains(addr, ":") {
addr += ":80" // 强制转换为 TCP 80 端口探测！
}

for i := 0; i < p.Count; i++ {
start := time.Now()
conn, err := net.DialTimeout("tcp", addr, p.Timeout)
if err == nil {
rtt := time.Since(start)
total += rtt
success++
conn.Close()
if p.OnRecv != nil {
p.OnRecv(nil)
}
}
time.Sleep(p.Interval)
}

p.stats.PacketsRecv = success
p.stats.PacketLoss = float64(p.Count-success) / float64(p.Count) * 100.0
if success > 0 {
p.stats.AvgRtt = total / time.Duration(success)
}
if p.OnFinish != nil {
p.OnFinish(&p.stats)
}
return nil
}

func (p *Pinger) Statistics() *Statistics {
return &p.stats
}
