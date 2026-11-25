package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

func defaultReturnRouteTargets() []common.ReturnRouteTarget {
	enabled := true
	return []common.ReturnRouteTarget{
		{Carrier: "CT", Label: "电信回程", Host: "gd-ct-v4.ip.zstaticcdn.com", IPVersion: "v4", Enabled: &enabled},
		{Carrier: "CU", Label: "联通回程", Host: "gd-cu-v4.ip.zstaticcdn.com", IPVersion: "v4", Enabled: &enabled},
		{Carrier: "CM", Label: "移动回程", Host: "gd-cm-v4.ip.zstaticcdn.com", IPVersion: "v4", Enabled: &enabled},
	}
}

func normalizeReturnRouteConfig(c *common.ReturnRouteConfig) {
	if c.IntervalMinutes < 5 || c.IntervalMinutes > 1440 {
		c.IntervalMinutes = 30
	}
	if c.ConfirmCount < 1 || c.ConfirmCount > 5 {
		c.ConfirmCount = 2
	}
	if len(c.Targets) == 0 {
		c.Targets = defaultReturnRouteTargets()
	}
	for i := range c.Targets {
		c.Targets[i].Carrier = strings.ToUpper(trimLimited(c.Targets[i].Carrier, 16))
		if c.Targets[i].Carrier == "" {
			c.Targets[i].Carrier = fmt.Sprintf("R%d", i+1)
		}
		c.Targets[i].Label = trimLimited(c.Targets[i].Label, 40)
		if c.Targets[i].Label == "" {
			c.Targets[i].Label = c.Targets[i].Carrier
		}
		c.Targets[i].Host = trimLimited(c.Targets[i].Host, 255)
		c.Targets[i].IPVersion = common.NormalizeIPVersion(c.Targets[i].IPVersion)
		if c.Targets[i].Enabled == nil {
			v := true
			c.Targets[i].Enabled = &v
		}
	}
}

func returnRouteForNode(cfg common.ReturnRouteConfig, nodeID string) common.ReturnRouteConfig {
	out := cfg
	out.Targets = make([]common.ReturnRouteTarget, 0, len(cfg.Targets))
	if !cfg.Enabled {
		return out
	}
	for _, t := range cfg.Targets {
		if !common.BoolEnabled(t.Enabled) || strings.TrimSpace(t.Host) == "" {
			continue
		}
		if t.NodeIDs == nil {
			out.Targets = append(out.Targets, t)
			continue
		}
		for _, id := range t.NodeIDs {
			if id == nodeID {
				out.Targets = append(out.Targets, t)
				break
			}
		}
	}
	return out
}

type routeTrackState struct {
	Carrier       string
	Label         string
	IPVersion     string
	CurrentLine   string
	PrevLine      string
	ChangedAt     int64
	CheckedAt     int64
	CandidateLine string
	CandidateHits int
	Hops          []string
}

type pendingRouteAlert struct {
	NodeID    string
	NodeName  string
	Group     string
	Carrier   string
	Label     string
	IPVersion string
	OldLine   string
	NewLine   string
	ChangedAt time.Time
}

type returnRouteStateMonitor struct {
	sync.Mutex
	states        map[string]*routeTrackState // key: nodeID|carrier|ipVersion
	pendingAlerts []pendingRouteAlert
}

var routeMonitor = returnRouteStateMonitor{
	states: make(map[string]*routeTrackState),
}

func routeStateKey(nodeID, carrier, ipVersion string) string {
	return nodeID + "|" + strings.ToUpper(carrier) + "|" + common.NormalizeIPVersion(ipVersion)
}

func (m *returnRouteStateMonitor) observe(n common.NodeConfig, routes []common.ReturnRouteResult, cfg common.ReturnRouteConfig, now time.Time) {
	if !cfg.Enabled || len(routes) == 0 {
		return
	}
	confirmCount := cfg.ConfirmCount
	if confirmCount < 1 {
		confirmCount = 2
	}

	m.Lock()
	defer m.Unlock()
	for _, r := range routes {
		carrier := strings.ToUpper(strings.TrimSpace(r.Carrier))
		if carrier == "" {
			continue
		}
		ipVer := common.NormalizeIPVersion(r.IPVersion)
		line := strings.TrimSpace(r.Line)
		if line == "" {
			line = "UNKNOWN"
		}
		key := routeStateKey(n.ID, carrier, ipVer)
		st := m.states[key]
		if st == nil {
			st = &routeTrackState{
				Carrier:     carrier,
				Label:       r.Label,
				IPVersion:   ipVer,
				CurrentLine: line,
				CheckedAt:   r.CheckedAt,
				Hops:        append([]string(nil), r.Hops...),
			}
			m.states[key] = st
			continue
		}

		// Ignore duplicate report with the exact same CheckedAt timestamp
		if r.CheckedAt > 0 && r.CheckedAt == st.CheckedAt {
			continue
		}
		st.Label = r.Label
		st.CheckedAt = r.CheckedAt
		st.Hops = append([]string(nil), r.Hops...)

		if line == "UNKNOWN" {
			continue
		}
		if st.CurrentLine == "" || st.CurrentLine == "UNKNOWN" {
			st.CurrentLine = line
			st.CandidateLine = ""
			st.CandidateHits = 0
			continue
		}
		if line == st.CurrentLine {
			st.CandidateLine = ""
			st.CandidateHits = 0
			continue
		}

		// Candidate route change detected; apply debounce confirmation
		if st.CandidateLine == line {
			st.CandidateHits++
		} else {
			st.CandidateLine = line
			st.CandidateHits = 1
		}
		if st.CandidateHits >= confirmCount {
			oldLine := st.CurrentLine
			st.PrevLine = oldLine
			st.CurrentLine = line
			st.ChangedAt = now.Unix()
			st.CandidateLine = ""
			st.CandidateHits = 0

			saveReturnRouteChange(n.ID, carrier, st.Label, ipVer, oldLine, line, st.Hops, now)
			if cfg.NotifyChange {
				m.pendingAlerts = append(m.pendingAlerts, pendingRouteAlert{
					NodeID:    n.ID,
					NodeName:  n.Name,
					Group:     n.Group,
					Carrier:   carrier,
					Label:     st.Label,
					IPVersion: ipVer,
					OldLine:   oldLine,
					NewLine:   line,
					ChangedAt: now,
				})
				if len(m.pendingAlerts) > 200 {
					m.pendingAlerts = m.pendingAlerts[len(m.pendingAlerts)-200:]
				}
			}
		}
	}
}

func (m *returnRouteStateMonitor) drainAlerts() []pendingRouteAlert {
	m.Lock()
	defer m.Unlock()
	if len(m.pendingAlerts) == 0 {
		return nil
	}
	out := append([]pendingRouteAlert(nil), m.pendingAlerts...)
	m.pendingAlerts = nil
	return out
}

func (m *returnRouteStateMonitor) enrich(nodeID string, reported []common.ReturnRouteResult) []common.ReturnRouteResult {
	if len(reported) == 0 {
		return nil
	}
	m.Lock()
	defer m.Unlock()
	out := make([]common.ReturnRouteResult, 0, len(reported))
	for _, r := range reported {
		item := r
		item.IPVersion = common.NormalizeIPVersion(item.IPVersion)
		key := routeStateKey(nodeID, item.Carrier, item.IPVersion)
		if st, ok := m.states[key]; ok {
			if st.CurrentLine != "" && st.CurrentLine != "UNKNOWN" {
				item.Line = st.CurrentLine
			}
			item.PrevLine = st.PrevLine
			item.ChangedAt = st.ChangedAt
		}
		out = append(out, item)
	}
	return out
}

func saveReturnRouteChange(nodeID, carrier, label, ipVersion, oldLine, newLine string, hops []string, now time.Time) {
	if db == nil {
		return
	}
	hopsJSON, _ := json.Marshal(hops)
	_, err := db.Exec(
		`INSERT INTO return_route_history(timestamp, server_id, carrier, label, ip_version, old_line, new_line, hops) VALUES(?,?,?,?,?,?,?,?)`,
		now.UTC().Format("2006-01-02 15:04:05"), nodeID, carrier, label, ipVersion, oldLine, newLine, string(hopsJSON),
	)
	if err != nil {
		log.Printf("save return route history: %v", err)
	}
}

type returnRouteHistoryItem struct {
	ID        int64    `json:"id"`
	Time      int64    `json:"time"`
	Carrier   string   `json:"carrier"`
	Label     string   `json:"label"`
	IPVersion string   `json:"ip_version"`
	OldLine   string   `json:"old_line"`
	NewLine   string   `json:"new_line"`
	Hops      []string `json:"hops,omitempty"`
}

func returnRouteHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("server_id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "缺少 server_id")
		return
	}
	if db == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": []returnRouteHistoryItem{}})
		return
	}
	rows, err := db.Query(
		`SELECT id, unixepoch(timestamp), carrier, label, ip_version, old_line, new_line, hops FROM return_route_history WHERE server_id=? ORDER BY id DESC LIMIT 50`,
		id,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusOK, map[string]interface{}{"items": []returnRouteHistoryItem{}})
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "读取回程变动历史失败")
		return
	}
	defer rows.Close()
	items := make([]returnRouteHistoryItem, 0)
	for rows.Next() {
		var item returnRouteHistoryItem
		var hopsRaw string
		if err := rows.Scan(&item.ID, &item.Time, &item.Carrier, &item.Label, &item.IPVersion, &item.OldLine, &item.NewLine, &hopsRaw); err != nil {
			continue
		}
		if hopsRaw != "" {
			_ = json.Unmarshal([]byte(hopsRaw), &item.Hops)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}
