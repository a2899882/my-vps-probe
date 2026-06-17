package common

// PingResult 代表某一个自定义目标单次的 Ping 结果
type PingResult struct {
TargetName   string    `json:"target_name"`   // 目标名称，如 "广东移动"
CurrentDelay float64   `json:"current_delay"` // 当前延迟 (ms)
LossRate     float64   `json:"loss_rate"`     // 当前丢包率 (%)
History      []int     `json:"history"`       // 最近30次的环形历史数组: 1为正常，0为丢包
}

// ServerStatus 代表一台 VPS 所有需要上报的实时动态数据
type ServerStatus struct {
ServerID       string       `json:"server_id"`        // 服务器唯一标识
CPUUsage       float64      `json:"cpu_usage"`        // CPU 使用率 (%)
MemTotal       uint64       `json:"mem_total"`        // 内存总量
MemUsed        uint64       `json:"mem_used"`         // 已用内存
DiskTotal      uint64       `json:"disk_total"`       // 硬盘总量
DiskUsed       uint64       `json:"disk_used"`        // 已用硬盘
NetInSpeed     uint64       `json:"net_in_speed"`     // 实时下载网速 (Bytes/s)
NetOutSpeed    uint64       `json:"net_out_speed"`    // 实时上传网速 (Bytes/s)
NetInTransfer  uint64       `json:"net_in_transfer"`  // 本月累计下载流量
NetOutTransfer uint64       `json:"net_out_transfer"` // 本月累计上传流量
PingStatuses   []PingResult `json:"ping_statuses"`    // 多点 Ping 的红绿条历史数据
}
