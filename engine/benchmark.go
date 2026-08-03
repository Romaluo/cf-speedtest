package engine

import (
	"fmt"
	"strings"
	"sync"

	"cf-speedtest/config"
	"cf-speedtest/geo"
	"cf-speedtest/model"
	"cf-speedtest/scorer"
)

// BenchmarkEngine 并发测速引擎
type BenchmarkEngine struct {
	cfg      *config.Config
	taskCh   chan model.Task
	resultCh chan model.IPResult
	resolver *geo.Resolver
	localISP string // 缓存本机运营商
	initOnce sync.Once
}

func NewBenchmarkEngine(cfg *config.Config, resolver *geo.Resolver) *BenchmarkEngine {
	return &BenchmarkEngine{
		cfg:      cfg,
		taskCh:   make(chan model.Task, cfg.Concurrency*2),
		resultCh: make(chan model.IPResult, cfg.Concurrency*2),
		resolver: resolver,
	}
}

// initLocalISP 检测本机运营商（懒加载，只执行一次）
func (e *BenchmarkEngine) initLocalISP() {
	e.initOnce.Do(func() {
		if e.resolver != nil {
			e.localISP = e.resolver.GetLocalISP()
		}
		if e.localISP == "" {
			e.localISP = "Unknown"
		}
	})
}

// Run 启动测速引擎，返回结果通道
func (e *BenchmarkEngine) Run(tasks []model.Task) <-chan model.IPResult {
	// 懒加载本机运营商检测（在 Run 开始时执行，避免首次检测阻塞 worker）
	e.initLocalISP()

	// 启动 worker 协程池
	var wg sync.WaitGroup
	for i := 0; i < e.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.worker()
		}()
	}

	// 异步投递任务
	go func() {
		for _, task := range tasks {
			e.taskCh <- task
		}
		close(e.taskCh)
	}()

	// 等待所有 worker 完成后关闭结果通道
	go func() {
		wg.Wait()
		close(e.resultCh)
	}()

	return e.resultCh
}

// worker 单个测速协程
func (e *BenchmarkEngine) worker() {
	for task := range e.taskCh {
		result := e.benchmark(task)
		e.resultCh <- result
	}
}

// benchmark 对单个 IP 执行完整测速流程
func (e *BenchmarkEngine) benchmark(task model.Task) model.IPResult {
	result := model.IPResult{
		IP:   task.IP,
		Port: task.Port,
	}

	// ISP 字段存的是"本机运营商"——用于按线路分线路出优选 IP
	result.ISP = e.localISP

	// CountryCode 字段存的是"优选 Cloudflare IP 所属国家"——供终端用户选择地区
	// 优先使用 IP 源自带的国家代码（如 #JP），更准确且避免 resolver 查询开销
	if task.CountryCode != "" {
		result.CountryCode = task.CountryCode
	} else if e.resolver != nil && e.resolver.IsEnabled() {
		info := e.resolver.Lookup(task.IP)
		if info != nil {
			// xdb 直接返回 CountryCode 字段（如 "US"/"CN"），优先使用
			if info.CountryCode != "" && info.CountryCode != "0" {
				result.CountryCode = strings.ToUpper(info.CountryCode)
			} else if info.Country != "" && info.Country != "0" {
				// 退化方案：将国家中文名转换为 ISO 代码
				result.CountryCode = scorer.GetCountryCode(info.Country)
			}
		}
	}
	if result.CountryCode == "" {
		result.CountryCode = "-"
	}

	// 阶段 1: TCP Ping
	tcpResult, err := TCPPing(task.IP, task.Port, e.cfg.TCPPingCount, e.cfg.TCPPingTimeout)
	if err != nil {
		result.Err = fmt.Errorf("tcp ping failed: %w", err)
		return result
	}
	result.TCPLatencyAvg = tcpResult.AvgLatency
	result.TCPLossRate = tcpResult.LossRate

	// 阶段 2: HTTP 延迟测试（绑定到被测 IP）— P1-6: 支持单节点重试
	var httpResult *HTTPingResult
	httpRetries := e.cfg.RetryCount
	if httpRetries < 0 {
		httpRetries = 0
	}
	httpConnect := e.cfg.HTTPConnectTimeout
	httpRead := e.cfg.HTTPTimeout
	if httpConnect <= 0 {
		httpConnect = httpRead
	}
	for attempt := 0; attempt <= httpRetries; attempt++ {
		httpResult, err = HTTPing(e.cfg.HTTPTarget, task.IP, task.Port, e.cfg.HTTPCount, httpConnect, httpRead)
		if err == nil {
			break
		}
	}
	if err != nil {
		result.Err = fmt.Errorf("http ping failed after %d attempts: %w", httpRetries+1, err)
		return result
	}
	result.HTTPLatencyAvg = httpResult.AvgLatency
	result.HTTPJitter = httpResult.Jitter
	result.HTTPStatusCode = httpResult.StatusCode

	// 阶段 3: 下载带宽测试（绑定到被测 IP）— P1-6: 支持单节点重试
	var dlResult *DownloadResult
	dlRetries := e.cfg.RetryCount
	if dlRetries < 0 {
		dlRetries = 0
	}
	dlConnect := e.cfg.DLConnectTimeout
	dlRead := e.cfg.DLReadTimeout
	if dlConnect <= 0 {
		dlConnect = e.cfg.DLTimeout
	}
	if dlRead <= 0 {
		dlRead = e.cfg.DLTimeout
	}
	for attempt := 0; attempt <= dlRetries; attempt++ {
		dlResult, err = DownloadTest(e.cfg.DLTarget, task.IP, task.Port, e.cfg.DLSize, dlConnect, dlRead)
		if err == nil {
			break
		}
	}
	if err != nil {
		result.DownloadSpeed = 0
	} else {
		result.DownloadSpeed = dlResult.SpeedMBps
	}

	return result
}
