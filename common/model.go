package common

type PingTask struct { Name string `json:"name"`; Host string `json:"host"` }
type NodeConfig struct { ID string `json:"id"`; Name string `json:"name"`; Token string `json:"token"`; ExpireDate string `json:"expire_date"`; Region string `json:"region"` }
type AppConfig struct { SiteName string `json:"site_name"`; AdminUser string `json:"admin_user"`; AdminPass string `json:"admin_pass"`; Nodes []NodeConfig `json:"nodes"`; PingTasks []PingTask `json:"ping_tasks"` }
type AgentInstruction struct { ServerName string `json:"server_name"`; PingTasks []PingTask `json:"ping_tasks"` }
type PingResult struct { TargetName string `json:"target_name"`; CurrentDelay float64 `json:"current_delay"`; LossRate float64 `json:"loss_rate"`; History []int `json:"history"` }
type ServerStatus struct {
ServerID string `json:"server_id"`
IsOnline bool `json:"is_online"`
Uptime uint64 `json:"uptime"`
Load1 float64 `json:"load_1"`
CPUCores int `json:"cpu_cores"`
SwapTotal uint64 `json:"swap_total"`
SwapUsed uint64 `json:"swap_used"`
CPUUsage float64 `json:"cpu_usage"`
MemTotal uint64 `json:"mem_total"`
MemUsed uint64 `json:"mem_used"`
DiskTotal uint64 `json:"disk_total"`
DiskUsed uint64 `json:"disk_used"`
NetInSpeed uint64 `json:"net_in_speed"`
NetOutSpeed uint64 `json:"net_out_speed"`
NetInTransfer uint64 `json:"net_in_transfer"`
NetOutTransfer uint64 `json:"net_out_transfer"`; CountryCode string `json:"country_code"`
PingStatuses []PingResult `json:"ping_statuses"`
}
