package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"cf-speedtest/model"

	"gopkg.in/yaml.v3"
)

// SupportedCountries 支持的国家/地区代码列表（ISO 3166-1 alpha-2）
var SupportedCountries = map[string]string{
	"CN": "中国", "US": "美国", "JP": "日本", "KR": "韩国",
	"SG": "新加坡", "HK": "香港", "TW": "台湾", "MO": "澳门",
	"DE": "德国", "UK": "英国", "FR": "法国", "CA": "加拿大",
	"AU": "澳大利亚", "RU": "俄罗斯", "IN": "印度", "BR": "巴西",
	"NL": "荷兰", "SE": "瑞典", "FI": "芬兰", "NO": "挪威",
	"DK": "丹麦", "CH": "瑞士", "IT": "意大利", "ES": "西班牙",
	"PT": "葡萄牙", "IE": "爱尔兰", "BE": "比利时", "LU": "卢森堡",
	"AT": "奥地利", "GR": "希腊", "PL": "波兰", "HU": "匈牙利",
	"CZ": "捷克", "RO": "罗马尼亚", "BG": "保加利亚", "HR": "克罗地亚",
	"RS": "塞尔维亚", "TR": "土耳其", "IL": "以色列", "AE": "阿联酋",
	"SA": "沙特阿拉伯", "QA": "卡塔尔", "KW": "科威特", "OM": "阿曼",
	"BH": "巴林", "EG": "埃及", "ZA": "南非", "NG": "尼日利亚",
	"KE": "肯尼亚", "MA": "摩洛哥", "AR": "阿根廷", "MX": "墨西哥",
	"CL": "智利", "CO": "哥伦比亚", "VE": "委内瑞拉", "PE": "秘鲁",
	"EC": "厄瓜多尔", "UY": "乌拉圭", "PY": "巴拉圭", "BO": "玻利维亚",
	"NZ": "新西兰", "MY": "马来西亚", "TH": "泰国", "VN": "越南",
	"PH": "菲律宾", "ID": "印度尼西亚", "KH": "柬埔寨", "LA": "老挝",
	"MM": "缅甸", "BD": "孟加拉国", "PK": "巴基斯坦", "AF": "阿富汗",
	"IR": "伊朗", "IQ": "伊拉克", "SY": "叙利亚", "LB": "黎巴嫩",
	"JO": "约旦", "PS": "巴勒斯坦", "YE": "也门", "LY": "利比亚",
	"TN": "突尼斯", "DZ": "阿尔及利亚", "SD": "苏丹", "ET": "埃塞俄比亚",
	"TZ": "坦桑尼亚", "UG": "乌干达", "RW": "卢旺达", "BI": "布隆迪",
	"CD": "刚果", "AO": "安哥拉", "GH": "加纳", "CI": "科特迪瓦",
	"TG": "多哥", "BJ": "贝宁", "NE": "尼日尔", "ML": "马里",
	"BF": "布基纳法索", "SN": "塞内加尔", "GM": "冈比亚", "MR": "毛里塔尼亚",
	"SL": "塞拉利昂", "LR": "利比里亚", "GN": "几内亚", "GW": "几内亚比绍",
	"CV": "佛得角", "ST": "圣多美", "GQ": "赤道几内亚", "GA": "加蓬",
	"CG": "刚果共和国", "DO": "多米尼加", "CU": "古巴", "BZ": "伯利兹",
	"GT": "危地马拉", "HN": "洪都拉斯", "SV": "萨尔瓦多", "NI": "尼加拉瓜",
	"CR": "哥斯达黎加", "PA": "巴拿马", "JM": "牙买加", "HT": "海地",
}

// IP 选择模式常量
const (
	IPSelectModeAuto   = "auto"   // 自动选择：根据用户运营商线路自动选择最优 IP
	IPSelectModeManual = "manual" // 手动选择：从用户指定的国家/地区中选择 IP
	IPSelectModeHybrid = "hybrid" // 混合选择：自动+手动各占50%权重
)

// 任务类型常量（用于清理策略匹配）
const (
	JobTypeBenchmark = "benchmark" // 测速任务
	JobTypePush      = "push"      // 推送任务
)

// CleanupStrategy 单个任务类型的清理策略
type CleanupStrategy struct {
	GC        bool `yaml:"gc"`         // 触发垃圾回收释放内存
	TempFiles bool `yaml:"temp_files"` // 清理临时文件
	Processes bool `yaml:"processes"`  // 终止残留子进程
	DBVacuum  bool `yaml:"db_vacuum"`  // 数据库 VACUUM 压缩
	Verify    bool `yaml:"verify"`     // 清理后验证资源回归基线
}

// CleanupConfig 资源清理机制配置
type CleanupConfig struct {
	Enable          bool                       `yaml:"enable"`           // 总开关
	Strategies      map[string]CleanupStrategy `yaml:"strategies"`       // 按任务类型的清理策略
	TempFiles       []string                   `yaml:"temp_files"`       // 固定路径临时文件
	TempPatterns    []string                   `yaml:"temp_patterns"`    // 通配符临时文件模式
	ProcessTimeout  time.Duration              `yaml:"process_timeout"`  // 进程终止等待超时
	VerifyResources bool                       `yaml:"verify_resources"` // 清理后验证资源指标
	MemoryThreshold float64                    `yaml:"memory_threshold"` // 期望释放的额外内存比例（0.0-1.0）
}

// Config 全局配置
type Config struct {
	// IP 段来源
	IPv4URL     string   `yaml:"ipv4_url"`      // https://www.cloudflare.com/ips-v4
	IPv4Enabled bool     `yaml:"ipv4_enabled"`  // 启用官方地址拉取
	IPv4Count   int      `yaml:"ipv4_count"`    // 官方地址随机拉取数量（0=使用默认采样）
	ExtraIPURLs []string `yaml:"extra_ip_urls"` // 额外的 IP 来源 URL 列表

	// IP 选择模式
	IPSelectMode      string   `yaml:"ip_select_mode"`      // IP 选择模式: auto(自动) / manual(手动) / hybrid(混合)
	IPSelectCountries []string `yaml:"ip_select_countries"` // 手动/混合模式下的国家/地区代码列表
	CIDRStatsPath     string   `yaml:"cidr_stats_path"`     // CIDR 权重统计文件路径（空=不启用动态权重）

	// 测速参数
	Concurrency        int           `yaml:"concurrency"`          // 并发协程数
	TCPPingCount       int           `yaml:"tcp_ping_count"`       // TCP Ping 次数
	TCPPingPorts       []int         `yaml:"tcp_ping_ports"`       // TCP Ping 端口列表（支持多端口）
	TCPPingTimeout     time.Duration `yaml:"tcp_ping_timeout"`     // TCP Ping 超时时间
	HTTPTarget         string        `yaml:"http_target"`          // HTTP 测试目标 URL
	HTTPCount          int           `yaml:"http_count"`           // HTTP 测试次数
	HTTPTimeout        time.Duration `yaml:"http_timeout"`         // HTTP 总超时时间
	HTTPConnectTimeout time.Duration `yaml:"http_connect_timeout"` // HTTP 连接超时（P2-7，0=使用 HTTPTimeout）
	DLTarget           string        `yaml:"dl_target"`            // 下载测试文件 URL
	DLTimeout          time.Duration `yaml:"dl_timeout"`           // 下载总超时时间
	DLConnectTimeout   time.Duration `yaml:"dl_connect_timeout"`   // 下载连接超时（P2-7，0=使用 DLTimeout）
	DLReadTimeout      time.Duration `yaml:"dl_read_timeout"`      // 下载读取超时（P2-7，0=使用 DLTimeout）
	DLSize             int64         `yaml:"dl_size"`              // 预期下载字节数（用于计算带宽）
	MaxIPs             int           `yaml:"max_ips"`              // 最大测试 IP 数量（0 表示不限制）

	// 重试策略（P1-6）
	RetryCount         int  `yaml:"retry_count"`          // 单节点测速失败时的重试次数（0=不重试）
	RetryBatchFallback bool `yaml:"retry_batch_fallback"` // 批次降级重试：当批次失败时降低并发重试

	// 评分权重
	WeightLatency   float64 `yaml:"weight_latency"`   // 延迟权重
	WeightLoss      float64 `yaml:"weight_loss"`      // 丢包率权重
	WeightBandwidth float64 `yaml:"weight_bandwidth"` // 带宽权重
	WeightJitter    float64 `yaml:"weight_jitter"`    // HTTP 抖动权重

	// 数据库存储
	MinScoreThreshold float64 `yaml:"min_score_threshold"` // 写入数据库的最低评分阈值（0.0~1.0，低于此值的 IP 不入库）
	MaxDBSize         int     `yaml:"max_db_size"`         // 数据库最大 IP 数量（0 表示不限制，超过时末位淘汰）

	// IP入库硬性规则（任一条件不满足则禁止入库）
	RuleMaxTCPLatency   int     `yaml:"rule_max_tcp_latency"`   // TCP延迟上限（毫秒，超过则拒绝）
	RuleMaxLossRate     float64 `yaml:"rule_max_loss_rate"`     // 丢包率上限（0.0~1.0，超过则拒绝）
	RuleMaxHTTPLatency  int     `yaml:"rule_max_http_latency"`  // HTTP延迟上限（毫秒，超过则拒绝）
	RuleMinDownloadMbps float64 `yaml:"rule_min_download_mbps"` // 下载带宽下限（Mbps，低于则拒绝）

	// Cloudflare DNS 推送
	CFAPIKey    string `yaml:"cf_api_key"`     // Cloudflare API Token
	CFZoneID    string `yaml:"cf_zone_id"`     // Cloudflare Zone ID
	CFDNSName   string `yaml:"cf_dns_name"`    // DNS 记录名称（如 ip.example.com）
	CFDNSTTL    int    `yaml:"cf_dns_ttl"`     // DNS 记录 TTL（秒，默认 300）
	CFOptions   string `yaml:"cf_dns_options"` // DNS 记录选项（如 proxied=true）
	CFPushCount int    `yaml:"cf_push_count"`  // 推送到 Cloudflare 的 IP 数量（前 N 个，0 表示不推送）

	// GitHub 推送
	GithubToken     string `yaml:"github_token"`      // GitHub Personal Access Token
	GithubRepo      string `yaml:"github_repo"`       // GitHub 仓库（如 user/repo）
	GithubFilePath  string `yaml:"github_file_path"`  // 仓库中的文件路径（如 IP.txt）
	GithubBranch    string `yaml:"github_branch"`     // 分支名（默认 main）
	GithubPushCount int    `yaml:"github_push_count"` // 推送到 GitHub 的 IP 数量（前 N 个，0 表示不推送）

	// WxPusher 微信通知（P2-8）
	WxPusherEnable   bool     `yaml:"wxpusher_enable"`    // 总开关
	WxPusherAppToken string   `yaml:"wxpusher_app_token"` // 应用 Token（在 wxpusher.zjiecode.com 后台获取）
	WxPusherTopicIDs []int    `yaml:"wxpusher_topic_ids"` // 话题 ID 列表（订阅该话题的用户均会收到）
	WxPusherUIDs     []string `yaml:"wxpusher_uids"`      // 指定用户 UID 列表（可选，与话题二选一或同时使用）

	// IP 风险等级过滤（P2-9）
	IPRiskFilterEnable   bool          `yaml:"ip_risk_filter_enable"`   // 总开关：DNS推送前过滤高风险 IP
	IPRiskScoreThreshold int           `yaml:"ip_risk_score_threshold"` // 风险分数阈值（0-100，>此值视为高风险并过滤，默认 70）
	IPRiskFilterTimeout  time.Duration `yaml:"ip_risk_filter_timeout"`  // 风险查询超时

	// 直连模式（P2-10）
	DirectModeEnable bool `yaml:"direct_mode_enable"` // 启用直连模式：禁用 HTTP/HTTPS 代理，强制直连

	// 输出
	TopN int `yaml:"top_n"` // 输出前 N 个结果

	// 运行模式
	DaemonMode    bool          `yaml:"daemon_mode"`    // 是否后台运行
	IPDBPath      string        `yaml:"ip_db_path"`     // IP 归属地数据库路径
	LogFile       string        `yaml:"log_file"`       // 日志文件路径
	Interval      int           `yaml:"interval"`       // 定时任务间隔（分钟，兼容旧配置）
	CollectTime   string        `yaml:"collect_time"`   // 数据采集时间（HH:MM 格式，支持逗号分隔多时间点如 "06:00,12:00,18:00"）
	PushInterval  int           `yaml:"push_interval"`  // 自动推送间隔（小时，每 N 小时重测并推送，0 表示不自动推送）
	DBPath        string        `yaml:"db_path"`        // SQLite 数据库文件路径
	IPExpireTime  time.Duration `yaml:"ip_expire_time"` // IP 结果过期时间
	DataRetention int           `yaml:"data_retention"` // 数据保留天数

	// IP 归属地精准验证（xdb 初筛 + cdn-cgi/trace 精准验证）
	TraceVerifyEnable      bool          `yaml:"trace_verify_enable"`      // 是否启用 cdn-cgi/trace 精准验证
	TraceVerifyConcurrency int           `yaml:"trace_verify_concurrency"` // 纠错验证并发数（独立于测速并发）
	TraceEndpoint          string        `yaml:"trace_endpoint"`           // trace 端点 URL
	TraceHTTPTimeout       time.Duration `yaml:"trace_http_timeout"`       // trace HTTP 超时
	TraceConnectTimeout    time.Duration `yaml:"trace_connect_timeout"`    // trace 连接超时
	GeoCorrectionsPath     string        `yaml:"geo_corrections_path"`     // 纠错文件路径

	// Web Dashboard
	WebEnable     bool          `yaml:"web_enable"`      // 是否启用 Web Dashboard
	WebHost       string        `yaml:"web_host"`        // Web 监听地址（0.0.0.0 表示内外网都可访问）
	WebPort       int           `yaml:"web_port"`        // Web 监听端口
	WebUsername   string        `yaml:"web_username"`    // 登录用户名
	WebPassword   string        `yaml:"web_password"`    // 登录密码（明文，建议部署后修改）
	WebSessionTTL time.Duration `yaml:"web_session_ttl"` // 会话有效期

	// 自动更新（P1：版本检查 + 后续阶段：下载/安装/重启）
	UpdateCheckEnable   bool          `yaml:"update_check_enable"`   // 是否启用更新检查
	UpdateCheckURL      string        `yaml:"update_check_url"`      // version.json 的 URL（HTTPS，推荐 raw.githubusercontent.com）
	UpdateCheckInterval time.Duration `yaml:"update_check_interval"` // 检查间隔（默认 24h，最小 1m）
	UpdateAutoDownload  bool          `yaml:"update_auto_download"`  // 检测到新版本时是否自动下载（不自动安装，默认 false）
	UpdateTempDir       string        `yaml:"update_temp_dir"`       // 下载/解压临时目录（空=系统默认 /tmp 或 %TEMP%）

	// 资源清理机制
	Cleanup CleanupConfig `yaml:"cleanup"` // 任务完成后资源清理配置
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		IPv4URL:                "https://www.cloudflare.com/ips-v4",
		IPv4Enabled:            true,
		IPv4Count:              100,
		ExtraIPURLs:            []string{"https://your-custom-ip-list-1.txt", "https://your-custom-ip-list-2.txt"}, // 请在 config.yaml 中替换为你的自定义 IP 采集地址
		Concurrency:            50,
		TCPPingCount:           4,
		TCPPingPorts:           []int{443},
		TCPPingTimeout:         3 * time.Second,
		HTTPTarget:             "https://www.cloudflare.com/cdn-cgi/trace",
		HTTPCount:              3,
		HTTPTimeout:            5 * time.Second,
		DLTarget:               "https://speed.cloudflare.com/__down?bytes=5242880",
		DLTimeout:              30 * time.Second,
		DLSize:                 5 * 1024 * 1024,
		MaxIPs:                 200,
		RetryCount:             1, // 单节点失败时重试1次
		RetryBatchFallback:     true,
		WeightLatency:          0.35,
		WeightLoss:             0.25,
		WeightBandwidth:        0.3,
		WeightJitter:           0.1,
		MinScoreThreshold:      80.0, // 100分制，80分以上才入库
		MaxDBSize:              2000,
		RuleMaxTCPLatency:      1000, // TCP延迟 > 1000ms 拒绝入库
		RuleMaxLossRate:        0.0,  // 丢包率 > 0.0 拒绝入库（任何丢包）
		RuleMaxHTTPLatency:     2000, // HTTP延迟 > 2000ms 拒绝入库
		RuleMinDownloadMbps:    0.1,  // 下载带宽 < 0.1 Mbps 拒绝入库
		CFAPIKey:               "",
		CFZoneID:               "",
		CFDNSName:              "",
		CFDNSTTL:               300,
		CFOptions:              "proxied=true",
		CFPushCount:            0,
		GithubToken:            "",
		GithubRepo:             "",
		GithubFilePath:         "IP.txt",
		GithubBranch:           "main",
		GithubPushCount:        0,
		WxPusherEnable:         false,
		WxPusherAppToken:       "",
		WxPusherTopicIDs:       nil,
		WxPusherUIDs:           nil,
		IPRiskFilterEnable:     false,
		IPRiskScoreThreshold:   70, // >70 视为高风险
		IPRiskFilterTimeout:    5 * time.Second,
		DirectModeEnable:       false,
		TopN:                   20,
		DaemonMode:             false,
		IPDBPath:               "ip2region.xdb",
		LogFile:                "cf-speedtest.log",
		Interval:               60,
		DBPath:                 "speedtest.db",
		IPExpireTime:           24 * time.Hour,
		DataRetention:          30,
		TraceVerifyEnable:      false, // 默认关闭 trace 验证（按需开启）
		TraceVerifyConcurrency: 10,    // 纠错验证默认并发 10
		TraceEndpoint:          "https://www.cloudflare.com/cdn-cgi/trace",
		TraceHTTPTimeout:       8 * time.Second,
		TraceConnectTimeout:    4 * time.Second,
		GeoCorrectionsPath:     "geo_corrections.txt",
		IPSelectMode:           IPSelectModeAuto,
		IPSelectCountries:      nil,
		CIDRStatsPath:          "cidr_stats.json",
		CollectTime:            "05:00",
		PushInterval:           4,
		WebEnable:              false,
		WebHost:                "0.0.0.0",
		WebPort:                8080,
		WebUsername:            "admin",
		WebPassword:            "admin",
		WebSessionTTL:          12 * time.Hour,
		UpdateCheckEnable:      true,
		UpdateCheckURL:         "https://raw.githubusercontent.com/Romaluo/cf-speedtest/main/version.json",
		UpdateCheckInterval:    24 * time.Hour,
		UpdateAutoDownload:     false,
		UpdateTempDir:          "",
		Cleanup: CleanupConfig{
			Enable: true,
			Strategies: map[string]CleanupStrategy{
				JobTypeBenchmark: {GC: true, TempFiles: true, Processes: true, DBVacuum: false, Verify: true},
				JobTypePush:      {GC: true, TempFiles: true, Processes: true, DBVacuum: true, Verify: true},
			},
			TempFiles:       []string{},
			TempPatterns:    []string{},
			ProcessTimeout:  5 * time.Second,
			VerifyResources: true,
			MemoryThreshold: 0.9, // 释放 90% 额外内存
		},
	}
}

// Validate 验证配置项的合法性
func (cfg *Config) Validate() error {
	// 验证 IP 选择模式
	mode := strings.ToLower(strings.TrimSpace(cfg.IPSelectMode))
	if mode != IPSelectModeAuto && mode != IPSelectModeManual && mode != IPSelectModeHybrid {
		return fmt.Errorf("ip_select_mode 必须为 'auto'、'manual' 或 'hybrid'，当前值: '%s'", cfg.IPSelectMode)
	}
	cfg.IPSelectMode = mode

	// 手动/混合模式验证
	if mode == IPSelectModeManual || mode == IPSelectModeHybrid {
		if len(cfg.IPSelectCountries) == 0 {
			return fmt.Errorf("ip_select_mode 为 '%s' 时，ip_select_countries 必须至少包含一个国家/地区代码", mode)
		}

		for _, code := range cfg.IPSelectCountries {
			code = strings.ToUpper(strings.TrimSpace(code))
			if len(code) != 2 {
				return fmt.Errorf("国家/地区代码格式错误: '%s'，必须为2位字母（如 US, CN, JP）", code)
			}
			if _, ok := SupportedCountries[code]; !ok {
				return fmt.Errorf("不支持的国家/地区代码: '%s'，请参考文档中的支持列表", code)
			}
		}
	} else {
		// 自动模式下，忽略手动设置的国家列表
		if len(cfg.IPSelectCountries) > 0 {
			cfg.IPSelectCountries = nil
		}
	}

	// 验证 TCP 端口配置
	if len(cfg.TCPPingPorts) == 0 {
		return fmt.Errorf("tcp_ping_ports 不能为空，至少需要配置一个端口")
	}

	// 验证官方地址拉取数量
	if cfg.IPv4Count < 0 {
		return fmt.Errorf("ipv4_count 不能为负数，当前值: %d", cfg.IPv4Count)
	}

	seen := make(map[int]bool)
	for _, port := range cfg.TCPPingPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("端口号 %d 无效，有效范围为 1-65535", port)
		}
		if seen[port] {
			return fmt.Errorf("端口号 %d 重复配置", port)
		}
		seen[port] = true

		if port <= 1023 {
			// 保留端口仅警告，不阻止（443 是默认端口）
			// 这里不做 error，只在调用方记录日志
		}
	}

	// 验证数据采集时间格式（HH:MM，支持逗号分隔多时间点如 "06:00,12:00,18:00"）
	if cfg.CollectTime != "" {
		for _, t := range strings.Split(cfg.CollectTime, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, err := time.Parse("15:04", t); err != nil {
				return fmt.Errorf("collect_time 格式错误: '%s'，必须为 HH:MM 格式（如 05:00 或 06:00,12:00,18:00）", t)
			}
		}
	}

	// 验证推送间隔（0-168 小时，即一周内）
	if cfg.PushInterval < 0 || cfg.PushInterval > 168 {
		return fmt.Errorf("push_interval 无效: %d，有效范围为 0-168 小时（0 表示不自动推送）", cfg.PushInterval)
	}

	// 验证 Web Dashboard 配置
	if cfg.WebEnable {
		if cfg.WebPort < 1 || cfg.WebPort > 65535 {
			return fmt.Errorf("web_port 无效: %d，有效范围为 1-65535", cfg.WebPort)
		}
		if strings.TrimSpace(cfg.WebUsername) == "" {
			return fmt.Errorf("web_enable 启用时 web_username 不能为空")
		}
		if cfg.WebPassword == "" {
			return fmt.Errorf("web_enable 启用时 web_password 不能为空")
		}
		if cfg.WebSessionTTL <= 0 {
			cfg.WebSessionTTL = 12 * time.Hour
		}
	}

	// 验证 WxPusher 配置（P2-8）
	if cfg.WxPusherEnable {
		if cfg.WxPusherAppToken == "" {
			return fmt.Errorf("wxpusher_enable 启用时 wxpusher_app_token 不能为空")
		}
		if len(cfg.WxPusherTopicIDs) == 0 && len(cfg.WxPusherUIDs) == 0 {
			return fmt.Errorf("wxpusher_enable 启用时 wxpusher_topic_ids 或 wxpusher_uids 至少需配置一项")
		}
	}

	// 验证自动更新配置
	if cfg.UpdateCheckEnable {
		if strings.TrimSpace(cfg.UpdateCheckURL) == "" {
			return fmt.Errorf("update_check_enable 启用时 update_check_url 不能为空")
		}
		// HTTPS 强制要求,但允许 HTTP 用于本地测试(localhost / 127.0.0.1)
		if !strings.HasPrefix(cfg.UpdateCheckURL, "https://") {
			if strings.HasPrefix(cfg.UpdateCheckURL, "http://localhost") ||
				strings.HasPrefix(cfg.UpdateCheckURL, "http://127.0.0.1") {
				// 本地测试模式,允许 HTTP
			} else {
				return fmt.Errorf("update_check_url 必须为 HTTPS 协议（当前: %s），本地测试可使用 http://localhost 或 http://127.0.0.1", cfg.UpdateCheckURL)
			}
		}
		// 检查间隔最小 1 分钟，最大 30 天（避免过于频繁或过长间隔）
		if cfg.UpdateCheckInterval < time.Minute {
			cfg.UpdateCheckInterval = 24 * time.Hour
		}
		if cfg.UpdateCheckInterval > 30*24*time.Hour {
			return fmt.Errorf("update_check_interval 无效: %v，最大 30 天", cfg.UpdateCheckInterval)
		}
	}

	// 验证 IP 风险过滤配置（P2-9）
	if cfg.IPRiskFilterEnable {
		if cfg.IPRiskScoreThreshold < 0 || cfg.IPRiskScoreThreshold > 100 {
			return fmt.Errorf("ip_risk_score_threshold 无效: %d，有效范围为 0-100", cfg.IPRiskScoreThreshold)
		}
		if cfg.IPRiskFilterTimeout <= 0 {
			cfg.IPRiskFilterTimeout = 5 * time.Second
		}
	}

	// 验证清理配置
	if cfg.Cleanup.Enable {
		if cfg.Cleanup.MemoryThreshold < 0 || cfg.Cleanup.MemoryThreshold > 1 {
			return fmt.Errorf("cleanup.memory_threshold 无效: %.2f，有效范围为 0.0-1.0", cfg.Cleanup.MemoryThreshold)
		}
		if cfg.Cleanup.ProcessTimeout < 0 {
			return fmt.Errorf("cleanup.process_timeout 不能为负数")
		}
		// 确保至少有一个任务类型的策略
		if len(cfg.Cleanup.Strategies) == 0 {
			cfg.Cleanup.Strategies = map[string]CleanupStrategy{
				JobTypeBenchmark: {GC: true, TempFiles: true, Processes: true, DBVacuum: false, Verify: true},
				JobTypePush:      {GC: true, TempFiles: true, Processes: true, DBVacuum: true, Verify: true},
			}
		}
	}

	return nil
}

// LoadConfig 从 YAML 文件加载配置
func LoadConfig(filepath string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := os.Stat(filepath); os.IsNotExist(err) {
		return cfg, nil
	}

	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// SaveConfig 保存配置到 YAML 文件
func SaveConfig(cfg *Config, filepath string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath, data, 0644)
}

// PassesHardRules 检查 IP 结果是否通过硬性入库规则
// 任一条件不满足则返回 false（禁止入库）
func (c *Config) PassesHardRules(r model.IPResult) bool {
	// 规则1: TCP延迟超过阈值
	if c.RuleMaxTCPLatency > 0 && r.TCPLatencyAvg.Milliseconds() > int64(c.RuleMaxTCPLatency) {
		return false
	}
	// 规则2: 丢包率超过阈值
	if r.TCPLossRate > c.RuleMaxLossRate {
		return false
	}
	// 规则3: HTTP延迟超过阈值
	if c.RuleMaxHTTPLatency > 0 && r.HTTPLatencyAvg.Milliseconds() > int64(c.RuleMaxHTTPLatency) {
		return false
	}
	// 规则4: 下载带宽低于阈值（DownloadSpeed 单位为 MB/s，转换为 Mbps: ×8）
	if c.RuleMinDownloadMbps > 0 {
		downloadMbps := r.DownloadSpeed * 8
		if downloadMbps < c.RuleMinDownloadMbps {
			return false
		}
	}
	return true
}
