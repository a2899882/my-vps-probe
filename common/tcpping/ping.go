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
IPAddr      *net.IPAddr
Addr        string
MinRtt      time.Duration
MaxRtt      time.Duration
AvgRtt      time.Duration
StdDevRtt   time.Duration
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
Timeout:  time.Second * 1, // 严格1秒超时，断流必抓
Interval: time.Millisecond * 300,
}, nil
}

func (p *Pinger) SetPrivileged(b bool) {}
func (p *Pinger) Stop() {}

func (p *Pinger) Run() error {
p.stats.PacketsSent = p.Count
p.stats.Addr = p.target
p.stats.IPAddr = &net.IPAddr{IP: net.ParseIP("1.1.1.1")} // 确保序列化不报错

success := 0
var total time.Duration
addr := p.target
if !strings.Contains(addr, ":") {
addr += ":80" // 强制转为 TCP 80
}

for i := 0; i < p.Count; i++ {
start := time.Now()
conn, err := net.DialTimeout("tcp", addr, p.Timeout)
if err == nil {
total += time.Since(start)
success++
conn.Close()
}
time.Sleep(p.Interval)
}

p.stats.PacketsRecv = success
p.stats.PacketLoss = float64(p.Count-success) / float64(p.Count) * 100.0
if success > 0 {
p.stats.AvgRtt = total / time.Duration(success)
}
return nil
}

func (p *Pinger) Statistics() *Statistics {
return &p.stats
}
