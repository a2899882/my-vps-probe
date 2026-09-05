package main

import (
	"testing"
	"time"

	psnet "github.com/shirou/gopsutil/v3/net"
	"my-vps-probe/common"
)

func TestNetworkRatesIgnoreInterfaceHistoryAndKeepOtherDirection(t *testing.T) {
	tracker := networkTracker{}
	now := time.Now()
	tracker.observe([]psnet.IOCountersStat{{Name: "eth0", BytesRecv: 1000, BytesSent: 2000}}, "boot-a", now, &common.ServerStatus{})
	status := common.ServerStatus{}
	tracker.observe([]psnet.IOCountersStat{{Name: "eth0", BytesRecv: 50, BytesSent: 2200}, {Name: "veth0", BytesRecv: 500000, BytesSent: 1000000}}, "boot-a", now.Add(2*time.Second), &status)
	if status.NetInSpeed != 0 || status.NetOutSpeed != 100 {
		t.Fatalf("counter reset or new interface created rate spike: %+v", status)
	}
	if status.NetInTransfer != 500050 || len(status.NetCounters) != 2 {
		t.Fatal("raw system totals changed scope")
	}
	status = common.ServerStatus{}
	tracker.observe([]psnet.IOCountersStat{{Name: "eth0", BytesRecv: 100000, BytesSent: 200000}}, "boot-b", now.Add(4*time.Second), &status)
	if status.NetInSpeed != 0 || status.NetOutSpeed != 0 {
		t.Fatal("reboot created rate spike")
	}
}
