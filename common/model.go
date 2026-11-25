package common

type PingTask struct {
	Name      string   `json:"name"`
	Host      string   `json:"host"`
	IPVersion string   `json:"ip_version,omitempty"` // "v4" or "v6", defaults to "v4"
	Enabled   *bool    `json:"enabled,omitempty"`    // nil means enabled
	NodeIDs   []string `json:"node_ids,omitempty"`
}

type PingConfig struct {
	EnableV4        *bool `json:"enable_v4"`
	EnableV6        *bool `json:"enable_v6"`
	IntervalSeconds int   `json:"interval_seconds"` // 5, 10, 30, or 60
}

type ReturnRouteTarget struct {
	Carrier   string   `json:"carrier"`              // "CT", "CU", "CM"
	Label     string   `json:"label"`                // e.g. "电信回程"
	Host      string   `json:"host"`                 // target IP or hostname
	IPVersion string   `json:"ip_version,omitempty"` // "v4" or "v6"
	Enabled   *bool    `json:"enabled,omitempty"`
	NodeIDs   []string `json:"node_ids,omitempty"`
}

type ReturnRouteConfig struct {
	Enabled         bool                `json:"enabled"`
	IntervalMinutes int                 `json:"interval_minutes"` // 10, 30, 60, 180
	NotifyChange    bool                `json:"notify_change"`
	ConfirmCount    int                 `json:"confirm_count"` // consecutive probes before confirming change
	NodeIDs         []string            `json:"node_ids,omitempty"` // nil means all nodes
	Targets         []ReturnRouteTarget `json:"targets"`
}

type ReturnRouteResult struct {
	Carrier   string   `json:"carrier"`
	Label     string   `json:"label"`
	IPVersion string   `json:"ip_version,omitempty"`
	Line      string   `json:"line"`
	PrevLine  string   `json:"prev_line,omitempty"`
	ChangedAt int64    `json:"changed_at,omitempty"`
	CheckedAt int64    `json:"checked_at,omitempty"`
	Hops      []string `json:"hops,omitempty"`
}

type NodeConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Token        string `json:"token"`
	ExpireDate   string `json:"expire_date"`
	RenewalCycle string `json:"renewal_cycle,omitempty"`
	AutoRenew    bool   `json:"auto_renew,omitempty"`
	Region       string `json:"region"`
	Group        string `json:"group"`
}

type AppConfig struct {
	SiteName      string            `json:"site_name"`
	AdminUser     string            `json:"admin_user"`
	AdminPass     string            `json:"admin_pass"`
	Nodes         []NodeConfig      `json:"nodes"`
	GroupOrder    []string          `json:"group_order,omitempty"`
	PingTasks     []PingTask        `json:"ping_tasks"`
	Ping          PingConfig        `json:"ping"`
	ReturnRoute   ReturnRouteConfig `json:"return_route"`
	PanelDomain   string            `json:"panel_domain,omitempty"`
	BlockIPAccess bool              `json:"block_ip_access,omitempty"`
}

type AgentInstruction struct {
	ServerName    string            `json:"server_name"`
	PingTasks     []PingTask        `json:"ping_tasks"`
	PingConfig    PingConfig        `json:"ping_config,omitempty"`
	ReturnRoute   ReturnRouteConfig `json:"return_route,omitempty"`
	ReportSeconds int               `json:"report_seconds,omitempty"`
}

type CardPingStatus struct {
	TargetName     string     `json:"target_name"`
	IPVersion      string     `json:"ip_version,omitempty"`
	History60      []*float64 `json:"history_60"`
	HistoryLoss60  []*float64 `json:"history_loss_60"`
	HistoryStart   int64      `json:"history_start"`
	SampleMinutes  int        `json:"sample_minutes"`
	SuccessMinutes int        `json:"success_minutes"`
	CurrentAt      int64      `json:"current_at"`
	HasCurrent     bool       `json:"has_current"`
	AvgDelay1H     float64    `json:"avg_delay_1h"`
	LossRate1H     float64    `json:"loss_rate_1h"`
	CurrentDelay   float64    `json:"current_delay"`
}

type ServerStatus struct {
	// Optional fields keep old agents compatible. A failed collection must not
	// be interpreted as a counter reset; per-interface counters avoid cancellation.
	NetworkValid     *bool               `json:"network_valid,omitempty"`
	BootID           string              `json:"boot_id,omitempty"`
	NetCounters      []NetCounter        `json:"net_counters,omitempty"`
	ServerID         string              `json:"server_id"`
	LastReport       int64               `json:"last_report"`
	IsOnline         bool                `json:"is_online"`
	Uptime           uint64              `json:"uptime"`
	Load1            float64             `json:"load_1"`
	CPUCores         int                 `json:"cpu_cores"`
	SwapTotal        uint64              `json:"swap_total"`
	SwapUsed         uint64              `json:"swap_used"`
	CPUUsage         float64             `json:"cpu_usage"`
	MemTotal         uint64              `json:"mem_total"`
	MemUsed          uint64              `json:"mem_used"`
	DiskTotal        uint64              `json:"disk_total"`
	DiskUsed         uint64              `json:"disk_used"`
	NetInSpeed       uint64              `json:"net_in_speed"`
	NetOutSpeed      uint64              `json:"net_out_speed"`
	NetInTransfer    uint64              `json:"net_in_transfer"`
	NetOutTransfer   uint64              `json:"net_out_transfer"`
	TCPConnections   uint64              `json:"tcp_connections"`
	UDPConnections   uint64              `json:"udp_connections"`
	CountryCode      string              `json:"country_code"`
	PingStatuses     []PingResult        `json:"ping_statuses"`
	CardPingStatuses []CardPingStatus    `json:"card_ping_statuses"`
	ReturnRoutes     []ReturnRouteResult `json:"return_routes,omitempty"`
}

type NetCounter struct {
	Name string `json:"name"`
	In   uint64 `json:"in"`
	Out  uint64 `json:"out"`
}

type PingResult struct {
	TargetName   string    `json:"target_name"`
	IPVersion    string    `json:"ip_version,omitempty"`
	CurrentDelay float64   `json:"current_delay"`
	AvgDelay     float64   `json:"avg_delay"`
	LossRate     float64   `json:"loss_rate"`
	SampledAt    int64     `json:"sampled_at,omitempty"`
	History      []float64 `json:"history"`
}

// NormalizeIPVersion returns "v6" for IPv6 variants and "v4" otherwise.
func NormalizeIPVersion(raw string) string {
	switch raw {
	case "v6", "V6", "ipv6", "IPv6", "6":
		return "v6"
	default:
		return "v4"
	}
}

// NormalizePingInterval clamps the ping interval to supported values: 5, 10, 30, 60.
func NormalizePingInterval(seconds int) int {
	switch seconds {
	case 5, 10, 30, 60:
		return seconds
	default:
		if seconds <= 0 {
			return 60
		}
		if seconds < 8 {
			return 5
		}
		if seconds < 20 {
			return 10
		}
		if seconds < 45 {
			return 30
		}
		return 60
	}
}

// BoolEnabled treats a nil *bool as enabled (true) for backward compatibility.
func BoolEnabled(p *bool) bool {
	return p == nil || *p
}
