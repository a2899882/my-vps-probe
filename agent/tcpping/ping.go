package ping

import (
"net"
"strings"
"time"
)

// 补全所有原版参数，严防 Agent 崩溃！
type Statistics struct {
PacketsRecv           int
PacketsSent           int
PacketsRecvDuplicates int
PacketLoss            float64
IPAddr                *net.IPAddr
Addr                  string
Rtts                  []time.Duration
MinRtt                time.Duration
MaxRtt                time.Duration
AvgRtt                time.Duration
StdDevRtt             time.Duration
}

type Pinger struct {
Count    int
Timeout  time.Duration
Interval time.Duration
Size     int
OnRecv   func(interface{})
OnFinish func(*Statistics)
stats    Statistics
target   string
done     chan bool
}

func NewPinger(addr string) (*Pinger, error) {
return &Pinger{
target:   addr,
Count:    3,
Timeout:  time.Second * 1, // 严格1秒超时，真实抓取断流！
Interval: time.Millisecond * 300,
done:     make(chan bool),
}, nil
}

func (p *Pinger) SetPrivileged(b bool) {}
func (p *Pinger) Stop() {
select {
case <-p.done:
default:
close(p.done)
}
}

func (p *Pinger) Run() error {
p.stats.PacketsSent = p.Count
p.stats.Addr = p.target
p.stats.IPAddr = &net.IPAddr{IP: net.ParseIP("1.1.1.1")}

success := 0
var total time.Duration
addr := p.target
if !strings.Contains(addr, ":") {
addr += ":80" // 强制 TCP 探测
}

for i := 0; i < p.Count; i++ {
select {
case <-p.done:
return nil
default:
}

start := time.Now()
conn, err := net.DialTimeout("tcp", addr, p.Timeout)
if err == nil {
rtt := time.Since(start)
total += rtt
p.stats.Rtts = append(p.stats.Rtts, rtt)
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
