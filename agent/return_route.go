package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

type routeHopInfo struct {
	IP  string
	ASN int
}

type returnRouteSampler struct {
	mu       sync.RWMutex
	running  bool
	lastRun  time.Time
	lastKey  string
	results  []common.ReturnRouteResult
	asnCache map[string]int
}

func newReturnRouteSampler() *returnRouteSampler {
	return &returnRouteSampler{
		asnCache: make(map[string]int),
	}
}

func returnRouteConfigKey(cfg common.ReturnRouteConfig) string {
	var b strings.Builder
	if cfg.Enabled {
		b.WriteString("1|")
	} else {
		b.WriteString("0|")
	}
	b.WriteString(strconv.Itoa(cfg.IntervalMinutes))
	b.WriteByte('|')
	for _, t := range cfg.Targets {
		if !common.BoolEnabled(t.Enabled) {
			continue
		}
		b.WriteString(t.Carrier)
		b.WriteByte(':')
		b.WriteString(t.Host)
		b.WriteByte(':')
		b.WriteString(common.NormalizeIPVersion(t.IPVersion))
		b.WriteByte(';')
	}
	return b.String()
}

func (s *returnRouteSampler) resultsFor(cfg common.ReturnRouteConfig, now time.Time) []common.ReturnRouteResult {
	if !cfg.Enabled || len(cfg.Targets) == 0 {
		s.mu.Lock()
		s.results = nil
		s.lastKey = ""
		s.mu.Unlock()
		return nil
	}
	interval := cfg.IntervalMinutes
	if interval < 5 || interval > 1440 {
		interval = 30
	}
	key := returnRouteConfigKey(cfg)

	s.mu.Lock()
	shouldRun := !s.running && (s.lastKey != key || s.lastRun.IsZero() || now.Sub(s.lastRun) >= time.Duration(interval)*time.Minute)
	if shouldRun {
		s.running = true
		targets := append([]common.ReturnRouteTarget(nil), cfg.Targets...)
		go s.runProbe(targets, key)
	}
	out := make([]common.ReturnRouteResult, len(s.results))
	copy(out, s.results)
	s.mu.Unlock()
	return out
}

func (s *returnRouteSampler) runProbe(targets []common.ReturnRouteTarget, key string) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	active := make([]common.ReturnRouteTarget, 0, len(targets))
	for _, t := range targets {
		if common.BoolEnabled(t.Enabled) && strings.TrimSpace(t.Host) != "" {
			active = append(active, t)
		}
	}
	results := make([]common.ReturnRouteResult, len(active))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, target := range active {
		wg.Add(1)
		go func(idx int, t common.ReturnRouteTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = s.probeTarget(t)
		}(i, target)
	}
	wg.Wait()

	s.mu.Lock()
	s.results = results
	s.lastRun = time.Now()
	s.lastKey = key
	s.mu.Unlock()
}

func (s *returnRouteSampler) probeTarget(t common.ReturnRouteTarget) common.ReturnRouteResult {
	ipVer := common.NormalizeIPVersion(t.IPVersion)
	ips := traceRouteIPs(t.Host, ipVer)
	hops := make([]routeHopInfo, 0, len(ips))
	hopSummary := make([]string, 0, len(ips))
	for _, ipStr := range ips {
		if !isPublicRouteIP(ipStr) {
			continue
		}
		asn := s.lookupASN(ipStr)
		hops = append(hops, routeHopInfo{IP: ipStr, ASN: asn})
		if len(hopSummary) < 12 {
			if asn > 0 {
				hopSummary = append(hopSummary, fmt.Sprintf("%s (AS%d)", ipStr, asn))
			} else {
				hopSummary = append(hopSummary, ipStr)
			}
		}
	}
	line := classifyReturnRouteHops(hops)
	label := strings.TrimSpace(t.Label)
	if label == "" {
		switch strings.ToUpper(t.Carrier) {
		case "CT":
			label = "电信回程"
		case "CU":
			label = "联通回程"
		case "CM":
			label = "移动回程"
		default:
			label = t.Carrier
		}
	}
	return common.ReturnRouteResult{
		Carrier:   strings.ToUpper(strings.TrimSpace(t.Carrier)),
		Label:     label,
		IPVersion: ipVer,
		Line:      line,
		CheckedAt: time.Now().Unix(),
		Hops:      hopSummary,
	}
}

var routeIPv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
var routeIPv6Pattern = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}\b`)

func traceRouteIPs(rawHost, ipVersion string) []string {
	host := strings.TrimSpace(rawHost)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()

	var out []byte
	var err error
	if ipVersion == "v6" {
		if bin, lookErr := exec.LookPath("traceroute6"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-n", "-q", "1", "-w", "1", "-m", "18", host).CombinedOutput()
		} else if bin, lookErr := exec.LookPath("traceroute"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-6", "-n", "-q", "1", "-w", "1", "-m", "18", host).CombinedOutput()
		} else if bin, lookErr := exec.LookPath("tracepath6"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-n", "-m", "18", host).CombinedOutput()
		} else if bin, lookErr := exec.LookPath("tracepath"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-6", "-n", "-m", "18", host).CombinedOutput()
		} else {
			return nil
		}
	} else {
		if bin, lookErr := exec.LookPath("traceroute"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-4", "-n", "-q", "1", "-w", "1", "-m", "18", host).CombinedOutput()
			if err != nil && len(out) == 0 {
				out, err = exec.CommandContext(ctx, bin, "-n", "-q", "1", "-w", "1", "-m", "18", host).CombinedOutput()
			}
		} else if bin, lookErr := exec.LookPath("tracepath"); lookErr == nil {
			out, err = exec.CommandContext(ctx, bin, "-n", "-m", "18", host).CombinedOutput()
		} else {
			return nil
		}
	}
	if len(out) == 0 && err != nil {
		return nil
	}

	lines := strings.Split(string(out), "\n")
	seen := make(map[string]bool)
	var ips []string
	for idx, line := range lines {
		line = strings.TrimSpace(line)
		// Skip traceroute header line ("traceroute to ...")
		if idx == 0 && strings.HasPrefix(strings.ToLower(line), "traceroute") {
			continue
		}
		var match string
		if ipVersion == "v6" {
			for _, cand := range routeIPv6Pattern.FindAllString(line, -1) {
				if ip := net.ParseIP(cand); ip != nil && ip.To4() == nil {
					match = ip.String()
					break
				}
			}
		} else {
			for _, cand := range routeIPv4Pattern.FindAllString(line, -1) {
				if ip := net.ParseIP(cand); ip != nil && ip.To4() != nil {
					match = ip.String()
					break
				}
			}
		}
		if match != "" && !seen[match] {
			seen[match] = true
			ips = append(ips, match)
		}
	}
	return ips
}

func isPublicRouteIP(raw string) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// Exclude RFC6598 CGNAT 100.64.0.0/10
		if v4[0] == 100 && v4[1]&0xc0 == 0x40 {
			return false
		}
	}
	return true
}

func (s *returnRouteSampler) lookupASN(ipStr string) int {
	s.mu.RLock()
	if cached, ok := s.asnCache[ipStr]; ok {
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	asn := queryCymruASN(ipStr)
	s.mu.Lock()
	if len(s.asnCache) > 2048 {
		s.asnCache = make(map[string]int)
	}
	s.asnCache[ipStr] = asn
	s.mu.Unlock()
	return asn
}

func queryCymruASN(ipStr string) int {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0
	}
	var queryName string
	if v4 := ip.To4(); v4 != nil {
		queryName = fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", v4[3], v4[2], v4[1], v4[0])
	} else {
		v6 := ip.To16()
		if v6 == nil {
			return 0
		}
		nibbles := make([]string, 0, 32)
		for i := len(v6) - 1; i >= 0; i-- {
			nibbles = append(nibbles, fmt.Sprintf("%x", v6[i]&0x0f))
			nibbles = append(nibbles, fmt.Sprintf("%x", v6[i]>>4))
		}
		queryName = strings.Join(nibbles, ".") + ".origin6.asn.cymru.com"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	txts, err := net.DefaultResolver.LookupTXT(ctx, queryName)
	if err != nil || len(txts) == 0 {
		return 0
	}
	parts := strings.Split(txts[0], "|")
	if len(parts) == 0 {
		return 0
	}
	fields := strings.Fields(strings.TrimSpace(parts[0]))
	if len(fields) == 0 {
		return 0
	}
	asn, err := strconv.Atoi(fields[0])
	if err != nil || asn <= 0 {
		return 0
	}
	return asn
}

// classifyReturnRouteHops mirrors Lite's return-route signature priority:
// CUG VIP / 9929 / CMIN2 / CMI / CN2 GIA / CN2 GT / 163 / 4837 / CMNET.
func classifyReturnRouteHops(hops []routeHopInfo) string {
	if len(hops) == 0 {
		return "UNKNOWN"
	}

	hasCUG := false
	has9929 := false
	has4837 := false
	for _, h := range hops {
		if h.ASN == 10099 {
			hasCUG = true
		}
		if h.ASN == 9929 || isPrefixMatch(h.IP, "218.105.", "210.51.", "210.13.") {
			has9929 = true
		}
		if h.ASN == 4837 || isPrefixMatch(h.IP, "219.158.") {
			has4837 = true
		}
	}

	for _, h := range hops {
		if h.ASN == 10099 {
			if has9929 {
				return "CUG VIP"
			}
			if has4837 {
				return "CUG"
			}
		}
		if h.ASN == 9929 || isPrefixMatch(h.IP, "218.105.", "210.51.", "210.13.") {
			if hasCUG {
				return "CUG VIP"
			}
			return "9929"
		}
		if h.ASN == 58807 || isPrefixMatch(h.IP, "58.254.") {
			return "CMIN2"
		}
		if h.ASN == 58453 || isPrefixMatch(h.IP, "223.120.", "223.118.", "223.119.") {
			return "CMI"
		}
	}

	// CN2 GIA vs CN2 GT (AS4809 / 59.43.0.0/16 / AS23764)
	firstCN2 := -1
	for i, h := range hops {
		if h.ASN == 4809 || isPrefixMatch(h.IP, "59.43.") {
			firstCN2 = i
			break
		}
	}
	if firstCN2 >= 0 {
		for i := 0; i < firstCN2; i++ {
			if hops[i].ASN == 23764 {
				return "CN2 GIA"
			}
		}
		cn2Count := 0
		backbone163After := 0
		for i := firstCN2; i < len(hops); i++ {
			if hops[i].ASN == 4809 || isPrefixMatch(hops[i].IP, "59.43.") {
				cn2Count++
			}
			if i > firstCN2 && isPrefixMatch(hops[i].IP, "202.97.") {
				backbone163After++
			}
		}
		if backbone163After >= 2 {
			return "CN2 GT"
		}
		if cn2Count >= 2 || backbone163After == 0 {
			return "CN2 GIA"
		}
		return "CN2 GT"
	}

	// Ordinary backbones: 163, 4837, CMNET
	for _, h := range hops {
		if h.ASN == 4134 || h.ASN == 4812 || isPrefixMatch(h.IP, "202.97.") {
			return "163"
		}
		if h.ASN == 4837 || isPrefixMatch(h.IP, "219.158.") {
			return "4837"
		}
		if h.ASN == 9808 || h.ASN == 56040 || h.ASN == 56041 || h.ASN == 56046 || isPrefixMatch(h.IP, "221.183.", "221.176.") {
			return "CMNET"
		}
	}
	return "UNKNOWN"
}

func isPrefixMatch(ip string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(ip, p) {
			return true
		}
	}
	return false
}
