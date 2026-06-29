package ping

import (
"net"
"strings"
"time"
)

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
Timeout:  time.Second * 1, // 严格1秒超时，断流必抓
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
p.stats.Rtts = make([]time.Duration, 0)

// 🌟 终极修复：真实解析 IP！保证小鸡测速后能完美映射回面板，红绿格子瞬间复活！
if ipaddr, err := net.ResolveIPAddr("ip", p.target); err == nil {
p.stats.IPAddr = ipaddr
} else {
p.stats.IPAddr = &net.IPAddr{IP: net.ParseIP("1.1.1.1")}
}

success := 0
var total time.Duration
addr := p.target
if !strings.Contains(addr, ":") {
addr += ":80" // 强制转为 TCP 80
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
