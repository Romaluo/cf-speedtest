package web

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cf-speedtest/config"
	"cf-speedtest/repository"
)

// ===== 基础接口 =====

func (srv *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"service": "cf-speedtest",
		"version": srv.deps.Version,
	})
}

func (srv *Server) systemHandler(w http.ResponseWriter, r *http.Request) {
	count, _ := srv.deps.DB.Count()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":       srv.deps.Version,
		"started_at":    srv.startAt.Format(time.RFC3339),
		"uptime_sec":    int(time.Since(srv.startAt).Seconds()),
		"config_path":   srv.deps.CfgPath,
		"db_path":       srv.deps.Cfg.DBPath,
		"log_file":      srv.deps.Cfg.LogFile,
		"geo_enabled":   srv.deps.Resolver.IsEnabled(),
		"db_count":      count,
		"daemon_mode":   srv.deps.Cfg.DaemonMode,
		"collect_time":  srv.deps.Cfg.CollectTime,
		"push_interval": srv.deps.Cfg.PushInterval,
	})
}

// ===== 结果查询 =====

// resultJSON 测速结果 JSON 视图（时长转为毫秒，便于前端展示）
type resultJSON struct {
	IP             string  `json:"ip"`
	Port           int     `json:"port"`
	CountryCode    string  `json:"country_code"`
	ISP            string  `json:"isp"`
	TCPLatencyMs   float64 `json:"tcp_latency_ms"`
	TCPLossRate    float64 `json:"tcp_loss_rate"`
	HTTPLatencyMs  float64 `json:"http_latency_ms"`
	HTTPStatusCode int     `json:"http_status_code"`
	DownloadSpeed  float64 `json:"download_speed"` // MB/s
	DownloadMbps   float64 `json:"download_mbps"`  // Mbps
	Score          float64 `json:"score"`
	UpdatedAt      string  `json:"updated_at"`
}

func toResultJSON(r repository.ResultRow) resultJSON {
	return resultJSON{
		IP:             r.IP,
		Port:           r.Port,
		CountryCode:    r.CountryCode,
		ISP:            r.ISP,
		TCPLatencyMs:   float64(r.TCPLatencyAvg.Milliseconds()),
		TCPLossRate:    r.TCPLossRate,
		HTTPLatencyMs:  float64(r.HTTPLatencyAvg.Milliseconds()),
		HTTPStatusCode: r.HTTPStatusCode,
		DownloadSpeed:  r.DownloadSpeed,
		DownloadMbps:   r.DownloadSpeed * 8,
		Score:          r.Score,
		UpdatedAt:      r.UpdatedAt.Format(time.RFC3339),
	}
}

func (srv *Server) listResultsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 50)
	offset := parseIntDefault(q.Get("offset"), 0)
	minScore := parseFloatDefault(q.Get("min_score"), 0)

	f := repository.ResultFilter{
		Limit:    limit,
		Offset:   offset,
		ISP:      q.Get("isp"),
		Country:  q.Get("country"),
		MinPort:  parseIntDefault(q.Get("port_min"), 0),
		MaxPort:  parseIntDefault(q.Get("port_max"), 0),
		MinScore: minScore,
		Sort:     q.Get("sort"),
		Order:    q.Get("order"),
	}

	rows, total, err := srv.deps.DB.ListResults(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询失败: "+err.Error())
		return
	}

	data := make([]resultJSON, 0, len(rows))
	for _, row := range rows {
		data = append(data, toResultJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data":   data,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (srv *Server) topResultsHandler(w http.ResponseWriter, r *http.Request) {
	n := parseIntDefault(r.URL.Query().Get("n"), srv.deps.Cfg.TopN)
	if n <= 0 || n > 10000 {
		n = srv.deps.Cfg.TopN
	}
	results, err := srv.deps.DB.GetTopResults(n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询失败: "+err.Error())
		return
	}
	data := make([]resultJSON, 0, len(results))
	for _, ip := range results {
		data = append(data, resultJSON{
			IP:             ip.IP,
			Port:           ip.Port,
			CountryCode:    ip.CountryCode,
			ISP:            ip.ISP,
			TCPLatencyMs:   float64(ip.TCPLatencyAvg.Milliseconds()),
			TCPLossRate:    ip.TCPLossRate,
			HTTPLatencyMs:  float64(ip.HTTPLatencyAvg.Milliseconds()),
			HTTPStatusCode: ip.HTTPStatusCode,
			DownloadSpeed:  ip.DownloadSpeed,
			DownloadMbps:   ip.DownloadSpeed * 8,
			Score:          ip.Score,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": data})
}

func (srv *Server) deleteResultHandler(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	portStr := r.PathValue("port")
	if ip == "" || portStr == "" {
		writeError(w, http.StatusBadRequest, "缺少 ip 或 port")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "端口格式错误")
		return
	}
	if err := srv.deps.DB.DeleteResult(ip, port); err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	srv.deps.Logger.Info("WEB", "删除结果 %s:%d", ip, port)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (srv *Server) clearResultsHandler(w http.ResponseWriter, r *http.Request) {
	deleted, err := srv.deps.DB.DeleteAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "清空失败: "+err.Error())
		return
	}
	srv.deps.Logger.Info("WEB", "清空全部结果，共 %d 条", deleted)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": deleted})
}

// ===== 统计与维度 =====

func (srv *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	stats, err := srv.deps.DB.GetStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "统计失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (srv *Server) dimensionsHandler(w http.ResponseWriter, r *http.Request) {
	isps, _ := srv.deps.DB.DistinctISPs()
	countries, _ := srv.deps.DB.DistinctCountries()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"isps":          isps,
		"countries":     countries,
		"ports":         srv.deps.Cfg.TCPPingPorts,
		"all_countries": allCountryCodes(),
	})
}

func allCountryCodes() []map[string]string {
	out := make([]map[string]string, 0, len(config.SupportedCountries))
	for code, name := range config.SupportedCountries {
		out = append(out, map[string]string{"code": code, "name": name})
	}
	return out
}

// ===== 推送器状态 =====

func (srv *Server) pushersStatusHandler(w http.ResponseWriter, r *http.Request) {
	cfg := srv.deps.Cfg
	cfReady := cfg.CFPushCount > 0 && cfg.CFAPIKey != "" && cfg.CFZoneID != "" && cfg.CFDNSName != ""
	ghReady := cfg.GithubPushCount > 0 && cfg.GithubToken != "" && cfg.GithubRepo != ""

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cloudflare": map[string]interface{}{
			"enabled":    cfg.CFPushCount > 0,
			"configured": cfReady,
			"zone_id":    maskStr(cfg.CFZoneID),
			"dns_name":   cfg.CFDNSName,
			"ttl":        cfg.CFDNSTTL,
			"options":    cfg.CFOptions,
			"push_count": cfg.CFPushCount,
			"has_token":  cfg.CFAPIKey != "",
		},
		"github": map[string]interface{}{
			"enabled":    cfg.GithubPushCount > 0,
			"configured": ghReady,
			"repo":       cfg.GithubRepo,
			"file_path":  cfg.GithubFilePath,
			"branch":     cfg.GithubBranch,
			"push_count": cfg.GithubPushCount,
			"has_token":  cfg.GithubToken != "",
		},
	})
}

func maskStr(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:3] + "***" + s[len(s)-3:]
}

// ===== 任务触发 =====

func (srv *Server) triggerBenchmarkHandler(w http.ResponseWriter, r *http.Request) {
	// daemon 激活时禁止手动触发,避免与定时任务冲突
	if srv.deps.Daemon != nil && srv.deps.Daemon.IsActive() {
		writeError(w, http.StatusConflict, "定时任务运行中,请先停止定时任务再手动操作")
		return
	}
	job, err := srv.startJob("benchmark", func(job *Job) {
		srv.runBenchmark(job)
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job.view())
}

func (srv *Server) triggerPushHandler(w http.ResponseWriter, r *http.Request) {
	// daemon 激活时禁止手动触发,避免与定时任务冲突
	if srv.deps.Daemon != nil && srv.deps.Daemon.IsActive() {
		writeError(w, http.StatusConflict, "定时任务运行中,请先停止定时任务再手动操作")
		return
	}
	job, err := srv.startJob("push", func(job *Job) {
		srv.runPush(job)
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job.view())
}

// startJob 串行化启动一个任务（同一时刻仅允许一个运行中的任务）
func (srv *Server) startJob(typ string, work func(*Job)) (*Job, error) {
	srv.triggerMu.Lock()
	defer srv.triggerMu.Unlock()

	if cur := srv.jobs.current(); cur != nil {
		return nil, fmt.Errorf("已有任务正在运行: %s（%s），请等待完成", cur.Type, cur.ID)
	}
	id := newJobID()
	job := srv.jobs.create(id, typ)

	// 在锁外执行工作，避免长时间持锁导致其他请求阻塞
	// 使用 recover() 防止 goroutine panic 导致整个进程崩溃
	go func() {
		defer func() {
			if r := recover(); r != nil {
				srv.deps.Logger.Error("WEB", "任务 [%s] panic: %v", id, r)
				job.fail(fmt.Sprintf("内部错误: %v", r))
			}
		}()
		work(job)
	}()
	return job, nil
}

func (srv *Server) listJobsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":    srv.jobs.list(),
		"running": srv.jobs.currentView(),
	})
}

func (srv *Server) getJobHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := srv.jobs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	writeJSON(w, http.StatusOK, job.view())
}

// cancelJobHandler POST /api/jobs/cancel
// 终止当前正在运行的任务
func (srv *Server) cancelJobHandler(w http.ResponseWriter, r *http.Request) {
	cur := srv.jobs.current()
	if cur == nil {
		writeError(w, http.StatusConflict, "当前无运行中的任务")
		return
	}
	cur.Cancel()
	srv.deps.Logger.Info("WEB", "用户请求终止任务: %s(%s)", cur.Type, cur.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "任务终止请求已发送",
		"job_id":  cur.ID,
		"type":    cur.Type,
	})
}

// ===== 日志 =====

func (srv *Server) logsHandler(w http.ResponseWriter, r *http.Request) {
	lines := parseIntDefault(r.URL.Query().Get("lines"), 200)
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	logPath := srv.deps.Cfg.LogFile
	file, err := os.Open(logPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "无法打开日志文件: "+err.Error())
		return
	}
	defer file.Close()

	// 读取全部行后取末尾 N 行（日志通常不大）
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var all []string
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	start := 0
	if len(all) > lines {
		start = len(all) - lines
	}
	tail := all[start:]
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lines": tail,
		"total": len(all),
		"shown": len(tail),
	})
}

// ===== 配置 =====

// configDTO 配置 JSON 视图（时长用字符串表达，便于前端编辑）
type configDTO struct {
	IPv4URL     string   `json:"ipv4_url"`
	IPv4Enabled bool     `json:"ipv4_enabled"`
	IPv4Count   int      `json:"ipv4_count"`
	ExtraIPURLs []string `json:"extra_ip_urls"`

	IPSelectMode      string   `json:"ip_select_mode"`
	IPSelectCountries []string `json:"ip_select_countries"`

	Concurrency        int     `json:"concurrency"`
	TCPPingCount       int     `json:"tcp_ping_count"`
	TCPPingPorts       []int   `json:"tcp_ping_ports"`
	TCPPingTimeout     string  `json:"tcp_ping_timeout"`
	HTTPTarget         string  `json:"http_target"`
	HTTPCount          int     `json:"http_count"`
	HTTPTimeout        string  `json:"http_timeout"`
	HTTPConnectTimeout string  `json:"http_connect_timeout"`
	DLTarget           string  `json:"dl_target"`
	DLTimeout          string  `json:"dl_timeout"`
	DLConnectTimeout   string  `json:"dl_connect_timeout"`
	DLReadTimeout      string  `json:"dl_read_timeout"`
	DLSizeMB           float64 `json:"dl_size_mb"` // 前端以 MB 为单位展示和编辑
	MaxIPs             int     `json:"max_ips"`

	RetryCount         int  `json:"retry_count"`
	RetryBatchFallback bool `json:"retry_batch_fallback"`

	WeightLatency   float64 `json:"weight_latency"`
	WeightLoss      float64 `json:"weight_loss"`
	WeightBandwidth float64 `json:"weight_bandwidth"`
	WeightJitter    float64 `json:"weight_jitter"`

	MinScoreThreshold float64 `json:"min_score_threshold"`
	MaxDBSize         int     `json:"max_db_size"`

	RuleMaxTCPLatency   int     `json:"rule_max_tcp_latency"`
	RuleMaxLossRate     float64 `json:"rule_max_loss_rate"`
	RuleMaxHTTPLatency  int     `json:"rule_max_http_latency"`
	RuleMinDownloadMbps float64 `json:"rule_min_download_mbps"`

	CFAPIKey    string `json:"cf_api_key"`
	CFZoneID    string `json:"cf_zone_id"`
	CFDNSName   string `json:"cf_dns_name"`
	CFDNSTTL    int    `json:"cf_dns_ttl"`
	CFOptions   string `json:"cf_dns_options"`
	CFPushCount int    `json:"cf_push_count"`

	GithubToken     string `json:"github_token"`
	GithubRepo      string `json:"github_repo"`
	GithubFilePath  string `json:"github_file_path"`
	GithubBranch    string `json:"github_branch"`
	GithubPushCount int    `json:"github_push_count"`

	WxPusherEnable   bool     `json:"wxpusher_enable"`
	WxPusherAppToken string   `json:"wxpusher_app_token"`
	WxPusherTopicIDs []int    `json:"wxpusher_topic_ids"`
	WxPusherUIDs     []string `json:"wxpusher_uids"`

	IPRiskFilterEnable   bool   `json:"ip_risk_filter_enable"`
	IPRiskScoreThreshold int    `json:"ip_risk_score_threshold"`
	IPRiskFilterTimeout  string `json:"ip_risk_filter_timeout"`

	DirectModeEnable bool `json:"direct_mode_enable"`

	TraceVerifyEnable      bool   `json:"trace_verify_enable"`
	TraceVerifyConcurrency int    `json:"trace_verify_concurrency"`
	TraceEndpoint          string `json:"trace_endpoint"`
	TraceHTTPTimeout       string `json:"trace_http_timeout"`
	TraceConnectTimeout    string `json:"trace_connect_timeout"`

	TopN int `json:"top_n"`

	DaemonMode    bool   `json:"daemon_mode"`
	IPDBPath      string `json:"ip_db_path"`
	LogFile       string `json:"log_file"`
	Interval      int    `json:"interval"`
	CollectTime   string `json:"collect_time"`
	PushInterval  int    `json:"push_interval"`
	DBPath        string `json:"db_path"`
	IPExpireTime  string `json:"ip_expire_time"`
	DataRetention int    `json:"data_retention"`

	WebEnable     bool   `json:"web_enable"`
	WebHost       string `json:"web_host"`
	WebPort       int    `json:"web_port"`
	WebUsername   string `json:"web_username"`
	WebPassword   string `json:"web_password"`
	WebSessionTTL string `json:"web_session_ttl"`

	// 自动更新
	UpdateCheckEnable   bool   `json:"update_check_enable"`   // 启用版本检查
	UpdateCheckURL      string `json:"update_check_url"`      // version.json 的 URL
	UpdateCheckInterval string `json:"update_check_interval"` // 检查间隔(字符串形式,如 24h)
	UpdateAutoDownload  bool   `json:"update_auto_download"`  // 检测到新版本时自动下载
	UpdateTempDir       string `json:"update_temp_dir"`       // 下载/解压临时目录
}

func cfgToDTO(c *config.Config) configDTO {
	return configDTO{
		IPv4URL:                c.IPv4URL,
		IPv4Enabled:            c.IPv4Enabled,
		IPv4Count:              c.IPv4Count,
		ExtraIPURLs:            c.ExtraIPURLs,
		IPSelectMode:           c.IPSelectMode,
		IPSelectCountries:      c.IPSelectCountries,
		Concurrency:            c.Concurrency,
		TCPPingCount:           c.TCPPingCount,
		TCPPingPorts:           c.TCPPingPorts,
		TCPPingTimeout:         durStr(c.TCPPingTimeout),
		HTTPTarget:             c.HTTPTarget,
		HTTPCount:              c.HTTPCount,
		HTTPTimeout:            durStr(c.HTTPTimeout),
		HTTPConnectTimeout:     durStr(c.HTTPConnectTimeout),
		DLTarget:               c.DLTarget,
		DLTimeout:              durStr(c.DLTimeout),
		DLConnectTimeout:       durStr(c.DLConnectTimeout),
		DLReadTimeout:          durStr(c.DLReadTimeout),
		DLSizeMB:               float64(c.DLSize) / (1024 * 1024), // 字节 → MB
		MaxIPs:                 c.MaxIPs,
		RetryCount:             c.RetryCount,
		RetryBatchFallback:     c.RetryBatchFallback,
		WeightLatency:          c.WeightLatency,
		WeightLoss:             c.WeightLoss,
		WeightBandwidth:        c.WeightBandwidth,
		WeightJitter:           c.WeightJitter,
		MinScoreThreshold:      c.MinScoreThreshold,
		MaxDBSize:              c.MaxDBSize,
		RuleMaxTCPLatency:      c.RuleMaxTCPLatency,
		RuleMaxLossRate:        c.RuleMaxLossRate,
		RuleMaxHTTPLatency:     c.RuleMaxHTTPLatency,
		RuleMinDownloadMbps:    c.RuleMinDownloadMbps,
		CFAPIKey:               c.CFAPIKey,
		CFZoneID:               c.CFZoneID,
		CFDNSName:              c.CFDNSName,
		CFDNSTTL:               c.CFDNSTTL,
		CFOptions:              c.CFOptions,
		CFPushCount:            c.CFPushCount,
		GithubToken:            c.GithubToken,
		GithubRepo:             c.GithubRepo,
		GithubFilePath:         c.GithubFilePath,
		GithubBranch:           c.GithubBranch,
		GithubPushCount:        c.GithubPushCount,
		WxPusherEnable:         c.WxPusherEnable,
		WxPusherAppToken:       c.WxPusherAppToken,
		WxPusherTopicIDs:       c.WxPusherTopicIDs,
		WxPusherUIDs:           c.WxPusherUIDs,
		IPRiskFilterEnable:     c.IPRiskFilterEnable,
		IPRiskScoreThreshold:   c.IPRiskScoreThreshold,
		IPRiskFilterTimeout:    durStr(c.IPRiskFilterTimeout),
		DirectModeEnable:       c.DirectModeEnable,
		TraceVerifyEnable:      c.TraceVerifyEnable,
		TraceVerifyConcurrency: c.TraceVerifyConcurrency,
		TraceEndpoint:          c.TraceEndpoint,
		TraceHTTPTimeout:       durStr(c.TraceHTTPTimeout),
		TraceConnectTimeout:    durStr(c.TraceConnectTimeout),
		TopN:                   c.TopN,
		DaemonMode:             c.DaemonMode,
		IPDBPath:               c.IPDBPath,
		LogFile:                c.LogFile,
		Interval:               c.Interval,
		CollectTime:            c.CollectTime,
		PushInterval:           c.PushInterval,
		DBPath:                 c.DBPath,
		IPExpireTime:           durStr(c.IPExpireTime),
		DataRetention:          c.DataRetention,
		WebEnable:              c.WebEnable,
		WebHost:                c.WebHost,
		WebPort:                c.WebPort,
		WebUsername:            c.WebUsername,
		WebPassword:            c.WebPassword,
		WebSessionTTL:          durStr(c.WebSessionTTL),
		UpdateCheckEnable:      c.UpdateCheckEnable,
		UpdateCheckURL:         c.UpdateCheckURL,
		UpdateCheckInterval:    durStr(c.UpdateCheckInterval),
		UpdateAutoDownload:     c.UpdateAutoDownload,
		UpdateTempDir:          c.UpdateTempDir,
	}
}

func dtoToCfg(d configDTO) (*config.Config, error) {
	c := config.DefaultConfig()
	c.IPv4URL = d.IPv4URL
	c.IPv4Enabled = d.IPv4Enabled
	c.IPv4Count = d.IPv4Count
	c.ExtraIPURLs = d.ExtraIPURLs
	c.IPSelectMode = d.IPSelectMode
	c.IPSelectCountries = d.IPSelectCountries
	c.Concurrency = d.Concurrency
	c.TCPPingCount = d.TCPPingCount
	c.TCPPingPorts = d.TCPPingPorts
	dur, err := parseDur(d.TCPPingTimeout)
	if err != nil {
		return nil, fmt.Errorf("tcp_ping_timeout 格式错误: %w", err)
	}
	c.TCPPingTimeout = dur
	c.HTTPTarget = d.HTTPTarget
	c.HTTPCount = d.HTTPCount
	dur, err = parseDur(d.HTTPTimeout)
	if err != nil {
		return nil, fmt.Errorf("http_timeout 格式错误: %w", err)
	}
	c.HTTPTimeout = dur
	// http_connect_timeout 允许空值,空值=0(使用 HTTPTimeout)
	if strings.TrimSpace(d.HTTPConnectTimeout) != "" {
		dur, err = parseDur(d.HTTPConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("http_connect_timeout 格式错误: %w", err)
		}
		c.HTTPConnectTimeout = dur
	}
	c.DLTarget = d.DLTarget
	dur, err = parseDur(d.DLTimeout)
	if err != nil {
		return nil, fmt.Errorf("dl_timeout 格式错误: %w", err)
	}
	c.DLTimeout = dur
	if strings.TrimSpace(d.DLConnectTimeout) != "" {
		dur, err = parseDur(d.DLConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("dl_connect_timeout 格式错误: %w", err)
		}
		c.DLConnectTimeout = dur
	}
	if strings.TrimSpace(d.DLReadTimeout) != "" {
		dur, err = parseDur(d.DLReadTimeout)
		if err != nil {
			return nil, fmt.Errorf("dl_read_timeout 格式错误: %w", err)
		}
		c.DLReadTimeout = dur
	}
	if d.DLSizeMB <= 0 {
		return nil, fmt.Errorf("dl_size_mb 必须大于 0")
	}
	c.DLSize = int64(d.DLSizeMB * 1024 * 1024) // MB → 字节
	c.MaxIPs = d.MaxIPs
	c.RetryCount = d.RetryCount
	c.RetryBatchFallback = d.RetryBatchFallback
	c.WeightLatency = d.WeightLatency
	c.WeightLoss = d.WeightLoss
	c.WeightBandwidth = d.WeightBandwidth
	c.WeightJitter = d.WeightJitter
	c.MinScoreThreshold = d.MinScoreThreshold
	c.MaxDBSize = d.MaxDBSize
	c.RuleMaxTCPLatency = d.RuleMaxTCPLatency
	c.RuleMaxLossRate = d.RuleMaxLossRate
	c.RuleMaxHTTPLatency = d.RuleMaxHTTPLatency
	c.RuleMinDownloadMbps = d.RuleMinDownloadMbps
	c.CFAPIKey = d.CFAPIKey
	c.CFZoneID = d.CFZoneID
	c.CFDNSName = d.CFDNSName
	c.CFDNSTTL = d.CFDNSTTL
	c.CFOptions = d.CFOptions
	c.CFPushCount = d.CFPushCount
	c.GithubToken = d.GithubToken
	c.GithubRepo = d.GithubRepo
	c.GithubFilePath = d.GithubFilePath
	c.GithubBranch = d.GithubBranch
	c.GithubPushCount = d.GithubPushCount
	c.WxPusherEnable = d.WxPusherEnable
	c.WxPusherAppToken = d.WxPusherAppToken
	c.WxPusherTopicIDs = d.WxPusherTopicIDs
	c.WxPusherUIDs = d.WxPusherUIDs
	c.IPRiskFilterEnable = d.IPRiskFilterEnable
	c.IPRiskScoreThreshold = d.IPRiskScoreThreshold
	dur, err = parseDur(d.IPRiskFilterTimeout)
	if err != nil {
		return nil, fmt.Errorf("ip_risk_filter_timeout 格式错误: %w", err)
	}
	c.IPRiskFilterTimeout = dur
	c.DirectModeEnable = d.DirectModeEnable
	c.TraceVerifyEnable = d.TraceVerifyEnable
	c.TraceVerifyConcurrency = d.TraceVerifyConcurrency
	c.TraceEndpoint = d.TraceEndpoint
	dur, err = parseDur(d.TraceHTTPTimeout)
	if err != nil {
		return nil, fmt.Errorf("trace_http_timeout 格式错误: %w", err)
	}
	c.TraceHTTPTimeout = dur
	dur, err = parseDur(d.TraceConnectTimeout)
	if err != nil {
		return nil, fmt.Errorf("trace_connect_timeout 格式错误: %w", err)
	}
	c.TraceConnectTimeout = dur
	if d.TopN > 0 {
		c.TopN = d.TopN
	}
	c.DaemonMode = d.DaemonMode
	c.IPDBPath = d.IPDBPath
	c.LogFile = d.LogFile
	c.Interval = d.Interval
	c.CollectTime = d.CollectTime
	c.PushInterval = d.PushInterval
	c.DBPath = d.DBPath
	dur, err = parseDur(d.IPExpireTime)
	if err != nil {
		return nil, fmt.Errorf("ip_expire_time 格式错误: %w", err)
	}
	c.IPExpireTime = dur
	c.DataRetention = d.DataRetention
	c.WebEnable = d.WebEnable
	c.WebHost = d.WebHost
	c.WebPort = d.WebPort
	c.WebUsername = d.WebUsername
	c.WebPassword = d.WebPassword
	dur, err = parseDur(d.WebSessionTTL)
	if err != nil {
		return nil, fmt.Errorf("web_session_ttl 格式错误: %w", err)
	}
	c.WebSessionTTL = dur
	// 自动更新字段
	c.UpdateCheckEnable = d.UpdateCheckEnable
	c.UpdateCheckURL = d.UpdateCheckURL
	if strings.TrimSpace(d.UpdateCheckInterval) != "" {
		dur, err = parseDur(d.UpdateCheckInterval)
		if err != nil {
			return nil, fmt.Errorf("update_check_interval 格式错误: %w", err)
		}
		c.UpdateCheckInterval = dur
	}
	c.UpdateAutoDownload = d.UpdateAutoDownload
	c.UpdateTempDir = d.UpdateTempDir
	return c, nil
}

func durStr(d time.Duration) string {
	return d.String()
}

func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空值")
	}
	return time.ParseDuration(s)
}

func (srv *Server) getConfigHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, cfgToDTO(srv.deps.Cfg))
}

func (srv *Server) setConfigHandler(w http.ResponseWriter, r *http.Request) {
	var dto configDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}

	newCfg, err := dtoToCfg(dto)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := newCfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "配置校验失败: "+err.Error())
		return
	}

	// 备份原配置文件
	warnings := []string{}
	if srv.deps.CfgPath != "" {
		if _, err := os.Stat(srv.deps.CfgPath); err == nil {
			bak := srv.deps.CfgPath + ".bak"
			if data, err := os.ReadFile(srv.deps.CfgPath); err == nil {
				_ = os.WriteFile(bak, data, 0644)
			}
		}
		// 保存（注意: YAML 注释会丢失，已通过 .bak 保留原始注释版本）
		if err := config.SaveConfig(newCfg, srv.deps.CfgPath); err != nil {
			writeError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
			return
		}
		warnings = append(warnings, "配置已保存，YAML 注释已丢失，原始带注释版本见 .bak 文件")
	}

	// 检测路径类变更（需重启生效）
	old := srv.deps.Cfg
	if newCfg.DBPath != old.DBPath {
		warnings = append(warnings, "db_path 已变更，需重启程序生效")
	}
	if newCfg.IPDBPath != old.IPDBPath {
		warnings = append(warnings, "ip_db_path 已变更，需重启程序生效")
	}
	if newCfg.LogFile != old.LogFile {
		warnings = append(warnings, "log_file 已变更，需重启程序生效")
	}
	if newCfg.DaemonMode != old.DaemonMode {
		warnings = append(warnings, "daemon_mode 已变更，需重启程序生效")
	}

	// 用新配置覆盖内存中的共享配置对象
	*srv.deps.Cfg = *newCfg

	// 会话 TTL 热更新
	srv.sessions.setTTL(newCfg.WebSessionTTL)

	srv.deps.Logger.Info("WEB", "配置已通过 Web 修改并保存")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"warnings": warnings,
		"config":   cfgToDTO(srv.deps.Cfg),
	})
}

// testConfigHandler 仅校验配置不保存
func (srv *Server) testConfigHandler(w http.ResponseWriter, r *http.Request) {
	var dto configDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	c, err := dtoToCfg(dto)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": err.Error()})
		return
	}
	if err := c.Validate(); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"valid": true})
}

// ===== 辅助 =====

func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func parseFloatDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return n
}
