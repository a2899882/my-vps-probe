package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
	trackers          = make(map[string]*PingTracker)
	serverAddr, token string
	globalCountryCode = "OT"
	countryMutex      sync.RWMutex
)

func startCountryLookup() {
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("https://api.country.is/")
		if err == nil {
			defer resp.Body.Close()
			var res struct {
				CountryCode string `json:"country"`
			}
			json.NewDecoder(resp.Body).Decode(&res)
			if res.CountryCode != "" {
				countryMutex.Lock()
				globalCountryCode = res.CountryCode
				countryMutex.Unlock()
			}
		}
	}()
}

func main() {
	defaultServer := os.Getenv("PROBE_SERVER")
	if defaultServer == "" {
		defaultServer = "localhost:8080"
	}
	defaultToken := os.Getenv("PROBE_TOKEN")
	if defaultToken == "" {
		defaultToken = "123"
	}
	flag.StringVar(&serverAddr, "server", defaultServer, "主控地址")
	flag.StringVar(&token, "token", defaultToken, "Token")
	flag.Parse()
	startCountryLookup()
	for {
		connectAndReport()
		time.Sleep(5 * time.Second)
	}
}

func connectAndReport() {
	endpoint, err := makeWebSocketURL(serverAddr, token)
	if err != nil {
		return
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(endpoint, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	networkState := networkTracker{}
	bootID, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	var instr common.AgentInstruction
	if err := conn.ReadJSON(&instr); err != nil {
		return
	}

	updates := make(chan common.AgentInstruction, 1)
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		for {
			var next common.AgentInstruction
			if err := conn.ReadJSON(&next); err != nil {
				return
			}
			select {
			case updates <- next:
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- next:
				default:
				}
			}
		}
	}()

	for {
		iterationStarted := time.Now()
		select {
		case instr = <-updates:
		case <-disconnected:
			return
		default:
		}
		countryMutex.RLock()
		status := common.ServerStatus{IsOnline: true, CountryCode: globalCountryCode, BootID: strings.TrimSpace(string(bootID))}
		countryMutex.RUnlock()
		if h, err := host.Info(); err == nil && h != nil {
			status.Uptime = h.Uptime
		}
		if l, err := load.Avg(); err == nil && l != nil {
			status.Load1 = l.Load1
		}
		if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 {
			status.CPUUsage = safeMetric(c[0], 0, 100)
		}
		if cores, err := cpu.Counts(true); err == nil {
			status.CPUCores = cores
		}
		if v, err := mem.VirtualMemory(); err == nil && v != nil {
			status.MemTotal = v.Total
			status.MemUsed = safeUsed(v.Total, v.Used, v.Available)
		}
		if s, err := mem.SwapMemory(); err == nil && s != nil {
			status.SwapTotal = s.Total
			status.SwapUsed = safeUsed(s.Total, s.Used, s.Free)
		}
		if d, err := disk.Usage("/"); err == nil && d != nil {
			status.DiskTotal = d.Total
			status.DiskUsed = safeUsed(d.Total, d.Used, d.Free)
		}
		validNetwork := false
		status.NetworkValid = &validNetwork
		if n, err := psnet.IOCounters(true); err == nil && len(n) > 0 {
			validNetwork = true
			networkState.observe(n, status.BootID, time.Now(), &status)
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

		pingProbes := runPingTasks(instr.PingTasks)
		var pingResults []common.PingResult
		for i, task := range instr.PingTasks {
			delay, success := pingProbes[i].delay, pingProbes[i].success
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
		status.TCPConnections = tcpSocketConnections()
		status.UDPConnections = udpSocketConnections()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := conn.WriteJSON(status); err != nil {
			return
		}
		reportSeconds := instr.ReportSeconds
		if reportSeconds < 2 || reportSeconds > 60 {
			reportSeconds = 3
		}
		// Keep reports start-to-start at the configured interval. Sleeping for a
		// full interval after collection made one-second Ping timeouts turn a
		// nominal 3-second report cadence into 4-5 seconds on NAT/slow networks.
		timer := time.NewTimer(reportWait(reportSeconds, iterationStarted, time.Now()))
		select {
		case instr = <-updates:
			if !timer.Stop() {
				<-timer.C
			}
		case <-disconnected:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func safeMetric(value, minValue, maxValue float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return minValue
	}
	return min(max(value, minValue), maxValue)
}

func safeUsed(total, used, available uint64) uint64 {
	if total == 0 {
		return 0
	}
	if used <= total {
		return used
	}
	if available <= total {
		return total - available
	}
	return 0
}

func reportWait(seconds int, started, now time.Time) time.Duration {
	if seconds < 2 || seconds > 60 {
		seconds = 3
	}
	wait := time.Duration(seconds)*time.Second - now.Sub(started)
	if wait < 0 {
		return 0
	}
	return wait
}

func makeWebSocketURL(raw, authToken string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty server address")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server address")
	}
	if strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	query := u.Query()
	query.Set("token", authToken)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

type pingProbe struct {
	delay   float64
	success bool
}

func runPingTasks(tasks []common.PingTask) []pingProbe {
	results := make([]pingProbe, len(tasks))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	for i, task := range tasks {
		wg.Add(1)
		go func(index int, target string) {
			defer wg.Done()
			limit <- struct{}{}
			results[index].delay, results[index].success = tcpPing(target)
			<-limit
		}(i, task.Host)
	}
	wg.Wait()
	return results
}

func tcpSocketConnections() uint64 {
	var count uint64
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && strings.HasSuffix(fields[0], ":") {
				count++
			}
		}
	}
	return count
}

func udpSocketConnections() uint64 {
	var count uint64
	for _, path := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) <= 1 {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) == 2 && parts[1] != "0000" {
				count++
			}
		}
	}
	return count
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
