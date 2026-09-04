package main

import (
	"net/url"
	"testing"
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

func TestMakeWebSocketURLRejectsEmptyAddress(t *testing.T) {
	if _, err := makeWebSocketURL("", "token"); err == nil {
		t.Fatal("expected an error")
	}
}
