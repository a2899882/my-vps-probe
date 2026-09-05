package main

import (
	"time"

	psnet "github.com/shirou/gopsutil/v3/net"
	"my-vps-probe/common"
)

type networkTracker struct {
	at       time.Time
	bootID   string
	counters map[string]common.NetCounter
}

func (t *networkTracker) observe(counters []psnet.IOCountersStat, bootID string, now time.Time, status *common.ServerStatus) {
	current := make(map[string]common.NetCounter, len(counters))
	var inDelta, outDelta uint64
	for _, c := range counters {
		counter := common.NetCounter{Name: c.Name, In: c.BytesRecv, Out: c.BytesSent}
		current[c.Name] = counter
		status.NetInTransfer += counter.In
		status.NetOutTransfer += counter.Out
		status.NetCounters = append(status.NetCounters, counter)
		if previous, ok := t.counters[c.Name]; ok {
			if counter.In >= previous.In {
				inDelta += counter.In - previous.In
			}
			if counter.Out >= previous.Out {
				outDelta += counter.Out - previous.Out
			}
		}
	}
	seconds := now.Sub(t.at).Seconds()
	reboot := bootID != "" && t.bootID != "" && bootID != t.bootID
	if !t.at.IsZero() && seconds > 0 && !reboot {
		status.NetInSpeed = uint64(float64(inDelta) / seconds)
		status.NetOutSpeed = uint64(float64(outDelta) / seconds)
	}
	t.at, t.bootID, t.counters = now, bootID, current
}
