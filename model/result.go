package model

import "time"

// IPResult 单个 IP 的测速结果
type IPResult struct {
	IP             string        `json:"ip"`
	Port           int           `json:"port"`             // 端口号
	CountryCode    string        `json:"country_code"`     // 国家代码
	ISP            string        `json:"isp"`              // 运营商（电信/联通/移动/铁通/教育网等）
	TCPLatencyAvg  time.Duration `json:"tcp_latency_avg"`  // TCP 平均延迟
	TCPLossRate    float64       `json:"tcp_loss_rate"`    // TCP 丢包率 0.0~1.0
	HTTPLatencyAvg time.Duration `json:"http_latency_avg"` // HTTP 平均延迟
	HTTPJitter     time.Duration `json:"http_jitter"`      // HTTP 抖动（延迟标准差）
	HTTPStatusCode int           `json:"http_status_code"` // HTTP 状态码
	DownloadSpeed  float64       `json:"download_speed"`   // 下载速度 (MB/s)
	Score          float64       `json:"score"`            // 综合评分
	Err            error         `json:"-"`                // 测速过程中的错误
}

// Task 测速任务
type Task struct {
	IP          string
	Port        int
	CountryCode string // IP源自带的国家代码（如 #JP），优先使用
	SourceCIDR  string // 采样来源CIDR（用于权重统计，不存入DB）
}
