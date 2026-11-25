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
	"runtime/debug"
	"strconv"
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
	History   []float64
	Last      float64
	Host      string
	IPVersion string
}

type pingSampler struct {
	bucket      int64
	taskKey     string
	lastProbeAt time.Time
	results     []common.PingResult
	trackers    map[string]*PingTracker
}

var (
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
			_ = json.NewDecoder(resp.Body).Decode(&res)
			if res.CountryCode != "" {
				countryMutex.Lock()
				globalCountryCode = res.CountryCode
				countryMutex.Unlock()
			}
		}
	}()
}

func main() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(75)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(24 << 20)
	}
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
	pings := pingSampler{bucket: -1, trackers: make(map[string]*PingTracker)}
	routes := newReturnRouteSampler()
	lastMetrics := common.ServerStatus{}
	for {
		connectAndReport(&pings, routes, &lastMetrics)
		time.Sleep(5 * time.Second)
	}
}

func connectAndReport(pings *pingSampler, routes *returnRouteSampler, lastMetrics *common.ServerStatus) {
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

	// Resource APIs can fail for a single sample while /proc is being updated or
	// when a constrained container briefly hides a controller. Keep the last
	// successfully collected values so one transient read does not render as 0%.
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
		carryResourceMetrics(&status, *lastMetrics)
		if h, err := host.Info(); err == nil && h != nil {
			status.Uptime = h.Uptime
		}
		if l, err := load.Avg(); err == nil && l != nil {
			if value, ok := boundedMetric(l.Load1, 0, 1e9); ok {
				status.Load1 = value
			}
		}
		if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 {
			if value, ok := boundedMetric(c[0], 0, 100); ok {
				status.CPUUsage = value
			}
		}
		if cores, err := cpu.Counts(true); err == nil && cores > 0 {
			status.CPUCores = cores
		}
		if v, err := mem.VirtualMemory(); err == nil && v != nil {
			if used, ok := memoryUsed(v.Total, v.Used, v.Available, v.Free); ok {
				status.MemTotal = v.Total
				status.MemUsed = used
			}
			// VirtualMemory reads SwapTotal/SwapFree from /proc/meminfo. That is
			// container-aware, unlike unix.Sysinfo used by SwapMemory, which can
			// leak host swap values into OpenVZ/LXC guests.
			if used, ok := usedFromFree(v.SwapTotal, v.SwapFree); ok {
				status.SwapTotal = v.SwapTotal
				status.SwapUsed = used
			}
		} else if s, swapErr := mem.SwapMemory(); swapErr == nil && s != nil {
			// Fallback for platforms without a readable /proc/meminfo.
			if used, ok := usedFromFree(s.Total, s.Free); ok {
				status.SwapTotal = s.Total
				status.SwapUsed = used
			}
		}
		if d, err := disk.Usage("/"); err == nil && d != nil {
			if used, ok := capacityUsed(d.Total, d.Used, d.Free); ok {
				status.DiskTotal = d.Total
				status.DiskUsed = used
			}
		}
		*lastMetrics = status
		validNetwork := false
		status.NetworkValid = &validNetwork
		if n, err := psnet.IOCounters(true); err == nil && len(n) > 0 {
			validNetwork = true
			networkState.observe(n, status.BootID, time.Now(), &status)
		}

		// Probe TCP Ping targets according to configured IPv4/IPv6 switches and
		// interval (5/10/30/60s), and attach asynchronous return-route results.
		status.PingStatuses = pings.resultsForConfig(instr.PingTasks, instr.PingConfig, time.Now())
		if routes != nil {
			status.ReturnRoutes = routes.resultsFor(instr.ReturnRoute, time.Now())
		}
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

func filterActivePingTasks(tasks []common.PingTask, cfg common.PingConfig) []common.PingTask {
	enableV4 := common.BoolEnabled(cfg.EnableV4)
	enableV6 := common.BoolEnabled(cfg.EnableV6)
	active := make([]common.PingTask, 0, len(tasks))
	for _, task := range tasks {
		if !common.BoolEnabled(task.Enabled) {
			continue
		}
		ver := common.NormalizeIPVersion(task.IPVersion)
		if ver == "v4" && !enableV4 {
			continue
		}
		if ver == "v6" && !enableV6 {
			continue
		}
		task.IPVersion = ver
		active = append(active, task)
	}
	return active
}

func pingTaskKey(tasks []common.PingTask, interval int) string {
	var key strings.Builder
	key.WriteString(strconv.Itoa(interval))
	key.WriteByte(0)
	for _, task := range tasks {
		key.WriteString(task.Name)
		key.WriteByte(0)
		key.WriteString(task.Host)
		key.WriteByte(0)
		key.WriteString(common.NormalizeIPVersion(task.IPVersion))
		key.WriteByte(0)
	}
	return key.String()
}

func allResultsFailed(results []common.PingResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.CurrentDelay >= 0 {
			return false
		}
	}
	return true
}

// resultsFor keeps backward compatibility with existing unit tests (60s default).
func (s *pingSampler) resultsFor(tasks []common.PingTask, now time.Time) []common.PingResult {
	return s.resultsForConfig(tasks, common.PingConfig{IntervalSeconds: 60}, now)
}

func (s *pingSampler) resultsForConfig(tasks []common.PingTask, cfg common.PingConfig, now time.Time) []common.PingResult {
	interval := common.NormalizePingInterval(cfg.IntervalSeconds)
	activeTasks := filterActivePingTasks(tasks, cfg)
	if len(activeTasks) == 0 {
		s.trackers = make(map[string]*PingTracker)
		s.results = nil
		s.taskKey = ""
		s.bucket = -1
		return nil
	}

	bucket := now.Unix() / int64(interval)
	key := pingTaskKey(activeTasks, interval)
	if s.bucket == bucket && s.taskKey == key {
		if interval >= 30 && !s.lastProbeAt.IsZero() && allResultsFailed(s.results) && now.Sub(s.lastProbeAt) >= 15*time.Second {
			probes := runPingTasks(activeTasks)
			s.lastProbeAt = now
			sampledAt := now.Unix()
			for i, task := range activeTasks {
				tracker, ok := s.trackers[task.Name]
				if !ok {
					continue
				}
				if probes[i].success {
					tracker.Last = probes[i].delay
				} else {
					tracker.Last = -1
				}
				if len(tracker.History) > 0 {
					tracker.History[len(tracker.History)-1] = tracker.Last
				}
				avg, loss := summarizePingHistory(tracker.History)
				if i < len(s.results) {
					s.results[i] = common.PingResult{
						TargetName:   task.Name,
						IPVersion:    common.NormalizeIPVersion(task.IPVersion),
						CurrentDelay: tracker.Last,
						AvgDelay:     avg,
						LossRate:     loss,
						SampledAt:    sampledAt,
						History:      append([]float64(nil), tracker.History...),
					}
				}
			}
		}
		return s.results
	}

	newTrackers := make(map[string]*PingTracker, len(activeTasks))
	for _, task := range activeTasks {
		ver := common.NormalizeIPVersion(task.IPVersion)
		if tracker, ok := s.trackers[task.Name]; ok && tracker.Host == task.Host && tracker.IPVersion == ver {
			newTrackers[task.Name] = tracker
		} else {
			newTrackers[task.Name] = &PingTracker{History: make([]float64, 0), Last: -1, Host: task.Host, IPVersion: ver}
		}
	}
	s.trackers = newTrackers
	s.results = buildPingResults(activeTasks, runPingTasks(activeTasks), s.trackers, now.Unix())
	s.bucket, s.taskKey, s.lastProbeAt = bucket, key, now
	return s.results
}

func buildPingResults(tasks []common.PingTask, probes []pingProbe, trackers map[string]*PingTracker, sampledAt int64) []common.PingResult {
	results := make([]common.PingResult, 0, len(tasks))
	for i, task := range tasks {
		tracker := trackers[task.Name]
		if probes[i].success {
			tracker.Last = probes[i].delay
		} else {
			tracker.Last = -1
		}
		tracker.History = append(tracker.History, tracker.Last)
		if len(tracker.History) > 60 {
			tracker.History = tracker.History[len(tracker.History)-60:]
		}
		avg, loss := summarizePingHistory(tracker.History)
		results = append(results, common.PingResult{
			TargetName:   task.Name,
			IPVersion:    common.NormalizeIPVersion(task.IPVersion),
			CurrentDelay: tracker.Last,
			AvgDelay:     avg,
			LossRate:     loss,
			SampledAt:    sampledAt,
			History:      append([]float64(nil), tracker.History...),
		})
	}
	return results
}

func summarizePingHistory(history []float64) (float64, float64) {
	var sum float64
	valid := 0
	for _, value := range history {
		if value >= 0 {
			sum += value
			valid++
		}
	}
	avg := 0.0
	if valid > 0 {
		avg = sum / float64(valid)
	}
	loss := 0.0
	if len(history) > 0 {
		loss = float64(len(history)-valid) / float64(len(history)) * 100
	}
	return avg, loss
}

func safeMetric(value, minValue, maxValue float64) float64 {
	result, ok := boundedMetric(value, minValue, maxValue)
	if !ok {
		return minValue
	}
	return result
}

func boundedMetric(value, minValue, maxValue float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return min(max(value, minValue), maxValue), true
}

func memoryUsed(total, used, available, free uint64) (uint64, bool) {
	if total == 0 {
		return 0, false
	}
	if used <= total {
		return used, true
	}
	if available <= total {
		return total - available, true
	}
	if free <= total {
		return total - free, true
	}
	return 0, false
}

func usedFromFree(total, free uint64) (uint64, bool) {
	if free > total {
		return 0, false
	}
	return total - free, true
}

func capacityUsed(total, used, free uint64) (uint64, bool) {
	if total == 0 {
		return 0, false
	}
	if used <= total {
		return used, true
	}
	return usedFromFree(total, free)
}

func carryResourceMetrics(dst *common.ServerStatus, previous common.ServerStatus) {
	dst.Uptime = previous.Uptime
	dst.Load1 = previous.Load1
	dst.CPUCores = previous.CPUCores
	dst.CPUUsage = previous.CPUUsage
	dst.MemTotal = previous.MemTotal
	dst.MemUsed = previous.MemUsed
	dst.SwapTotal = previous.SwapTotal
	dst.SwapUsed = previous.SwapUsed
	dst.DiskTotal = previous.DiskTotal
	dst.DiskUsed = previous.DiskUsed
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
		go func(index int, target, ipVer string) {
			defer wg.Done()
			limit <- struct{}{}
			results[index].delay, results[index].success = tcpPing(target, ipVer)
			<-limit
		}(i, task.Host, common.NormalizeIPVersion(task.IPVersion))
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

// normalizeTCPTarget handles bare IPv4, bare IPv6, bracketed IPv6, and host:port.
func normalizeTCPTarget(raw string) string {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	// Bare IPv6 literal like "240e::1" or "[240e::1]"
	trimmed := strings.Trim(addr, "[]")
	if ip := net.ParseIP(trimmed); ip != nil {
		return net.JoinHostPort(ip.String(), "80")
	}
	return net.JoinHostPort(trimmed, "80")
}

func tcpPing(host, ipVersion string) (float64, bool) {
	addr := normalizeTCPTarget(host)
	if addr == "" {
		return 0, false
	}
	network := "tcp4"
	if common.NormalizeIPVersion(ipVersion) == "v6" {
		network = "tcp6"
	}
	for attempt := 0; attempt < 2; attempt++ {
		start := time.Now()
		conn, err := net.DialTimeout(network, addr, 1500*time.Millisecond)
		if err == nil {
			conn.Close()
			ms := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0
			return ms, true
		}
		if attempt == 0 {
			time.Sleep(120 * time.Millisecond)
		}
	}
	return 0, false
}
