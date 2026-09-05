package main

import (
	"math"
	"net/url"
	"testing"
	"time"
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
	// releases underflow Used into a value close to MaxUint64.
	if got := safeUsed(1024, ^uint64(0)-99, 2048); got != 0 {
		t.Fatalf("underflowed memory used = %d", got)
	}
	if got := safeUsed(1024, 9000, 124); got != 900 {
		t.Fatalf("available fallback = %d", got)
	}
	if got := safeMetric(math.Inf(1), 0, 100); got != 0 {
		t.Fatalf("infinite metric = %v", got)
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
