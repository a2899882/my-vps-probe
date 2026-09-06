package main

import (
	"math"
	"net/url"
	"testing"
	"time"

	"my-vps-probe/common"
)

func TestMakeWebSocketURL(t *testing.T) {
	tests := []struct {
		name   string
		server string
		scheme string
		host   string
		path   string
	}{
		{name: "plain http address", server: "127.0.0.1:8080", scheme: "ws", host: "127.0.0.1:8080", path: "/ws"},
		{name: "https with base path", server: "https://probe.example.com/base/", scheme: "wss", host: "probe.example.com", path: "/base/ws"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := makeWebSocketURL(tt.server, "a+b&c")
			if err != nil {
				t.Fatalf("makeWebSocketURL: %v", err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse result: %v", err)
			}
			if u.Scheme != tt.scheme || u.Host != tt.host || u.Path != tt.path {
				t.Fatalf("unexpected URL: %s", raw)
			}
			if got := u.Query().Get("token"); got != "a+b&c" {
				t.Fatalf("token roundtrip = %q", got)
			}
		})
	}
}

func TestMetricGuardsHandleBrokenContainerCounters(t *testing.T) {
	// Affected OpenVZ/LXC kernels can expose Available > Total. Older gopsutil
	// releases underflow Used into a value close to MaxUint64. MemFree remains
	// guest-scoped and is the safe last fallback.
	if got, ok := memoryUsed(1024, ^uint64(0)-99, 2048, 560); !ok || got != 464 {
		t.Fatalf("OpenVZ memory used = %d, valid = %v", got, ok)
	}
	if got, ok := memoryUsed(1024, 9000, 124, 1000); !ok || got != 900 {
		t.Fatalf("available memory used = %d, valid = %v", got, ok)
	}
	if _, ok := memoryUsed(1024, 9000, 2048, 2048); ok {
		t.Fatal("all-invalid memory counters were accepted")
	}
	if got, ok := usedFromFree(1024, 124); !ok || got != 900 {
		t.Fatalf("used-from-free = %d, valid = %v", got, ok)
	}
	if _, ok := usedFromFree(1024, 2048); ok {
		t.Fatal("free greater than total was accepted")
	}
	if got, ok := usedFromFree(0, 0); !ok || got != 0 {
		t.Fatalf("zero swap should be valid: %d, %v", got, ok)
	}
	if got, ok := capacityUsed(1024, 400, 500); !ok || got != 400 {
		t.Fatalf("valid disk used changed: %d, %v", got, ok)
	}
	if got, ok := capacityUsed(1024, 9000, 124); !ok || got != 900 {
		t.Fatalf("disk free fallback = %d, %v", got, ok)
	}
	if got := safeMetric(math.Inf(1), 0, 100); got != 0 {
		t.Fatalf("infinite metric = %v", got)
	}
	if _, ok := boundedMetric(math.NaN(), 0, 100); ok {
		t.Fatal("NaN metric was accepted")
	}
}

func TestCarryResourceMetrics(t *testing.T) {
	previous := common.ServerStatus{
		Uptime: 7, Load1: 0.5, CPUCores: 2, CPUUsage: 12,
		MemTotal: 1024, MemUsed: 512, SwapTotal: 256, SwapUsed: 64,
		DiskTotal: 4096, DiskUsed: 1024,
	}
	var current common.ServerStatus
	carryResourceMetrics(&current, previous)
	if current.MemUsed != 512 || current.SwapTotal != 256 || current.DiskUsed != 1024 || current.CPUUsage != 12 {
		t.Fatalf("resource metrics were not carried: %+v", current)
	}
}

func TestPingSamplerUsesOneProbePerMinuteAndCountsZeroMilliseconds(t *testing.T) {
	trackers := map[string]*PingTracker{"TCP": {History: []float64{10, -1}, Last: -1, Host: "example:80"}}
	results := buildPingResults(
		[]common.PingTask{{Name: "TCP"}},
		[]pingProbe{{delay: 0, success: true}},
		trackers,
	)
	if len(results) != 1 || results[0].CurrentDelay != 0 || results[0].AvgDelay != 5 {
		t.Fatalf("zero-ms probe was not retained: %+v", results)
	}
	if math.Abs(results[0].LossRate-100.0/3.0) > 0.001 {
		t.Fatalf("loss rate = %v", results[0].LossRate)
	}

	sampler := pingSampler{minute: 100, taskKey: pingTaskKey([]common.PingTask{{Name: "TCP", Host: "example:80"}}), results: results}
	if got := sampler.resultsFor([]common.PingTask{{Name: "TCP", Host: "example:80"}}, time.Unix(100*60+30, 0)); &got[0] != &sampler.results[0] {
		t.Fatal("same minute did not reuse the cached probe")
	}
}

func TestReportWaitIncludesCollectionTime(t *testing.T) {
	started := time.Unix(100, 0)
	if got := reportWait(3, started, started.Add(900*time.Millisecond)); got != 2100*time.Millisecond {
		t.Fatalf("wait = %v", got)
	}
	if got := reportWait(3, started, started.Add(4*time.Second)); got != 0 {
		t.Fatalf("overrun wait = %v", got)
	}
}

func TestMakeWebSocketURLRejectsEmptyAddress(t *testing.T) {
	if _, err := makeWebSocketURL("", "token"); err == nil {
		t.Fatal("expected an error")
	}
}
