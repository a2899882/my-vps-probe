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
Count        int
Timeout      time.Duration
Interval     time.Duration
SetNetwork   string
Size         int
Debug        bool
RecordRtts   bool
OSHasRouting bool
host         string
stats        Statistics
}

func NewPinger(host string) (*Pinger, error) {
return &Pinger{
host:    host,
Count:   3,
Timeout: time.Second * 2,
}, nil
}

func (p *Pinger) SetPrivileged(b bool) {}

func (p *Pinger) Run() error {
p.stats.PacketsSent = p.Count
success := 0
var total time.Duration

target := p.host
if !strings.Contains(target, ":") {
target += ":80" // 强制使用 TCP 80 端口探测！
}

for i := 0; i < p.Count; i++ {
start := time.Now()
conn, err := net.DialTimeout("tcp", target, p.Timeout)
if err == nil {
total += time.Since(start)
success++
conn.Close()
}
if p.Interval > 0 {
time.Sleep(p.Interval)
} else {
time.Sleep(time.Millisecond * 100)
}
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
