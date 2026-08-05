package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"cf-speedtest/cleanup"
	"cf-speedtest/collector"
	"cf-speedtest/config"
	"cf-speedtest/engine"
	"cf-speedtest/geo"
	"cf-speedtest/log"
	"cf-speedtest/model"
	"cf-speedtest/output"
	"cf-speedtest/pusher"
	"cf-speedtest/repository"
	"cf-speedtest/scorer"
	"cf-speedtest/updater"
	"cf-speedtest/web"
)

var (
	configFile   = flag.String("config", "config.yaml", "配置文件路径")
	daemonMode   = flag.Bool("daemon", false, "后台运行模式")
	logFile      = flag.String("log", "", "日志文件路径")
	interval     = flag.Int("interval", 60, "定时任务间隔（分钟）")
	printVersion = flag.Bool("version", false, "打印版本信息")
	concurrency  = flag.Int("concurrency", 0, "并发协程数")
	topN         = flag.Int("top_n", 0, "输出前 N 个结果")
	maxIPs       = flag.Int("max_ips", 0, "最大测试 IP 数量")
	ipv4Only     = flag.Bool("ipv4_only", false, "仅测试 IPv4")
	ipSelectMode = flag.String("ip_mode", "", "IP选择模式: auto(自动) / manual(手动)")
	rerunAll     = flag.Bool("rerun_all", false, "全量重测数据库中的所有 IP 并推送")
	pushOnly     = flag.Bool("push_only", false, "仅执行推送（不测速）")
)

const version = "1.1.0"

var logger *log.Logger
var cleaner *cleanup.Cleaner
var cidrStats *collector.CIDRStats

func main() {
	// 顶层 recover：防止主 goroutine panic 导致进程无日志崩溃
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[FATAL] 进程崩溃: %v\n", r)
			// 尝试写入日志
			if logger != nil {
				logger.Error("PANIC", "进程崩溃: %v", r)
			}
			os.Exit(1)
		}
	}()

	flag.Parse()

	if *printVersion {
		fmt.Printf("cf-speedtest v%s\n", version)
		return
	}

	var err error
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	if *daemonMode {
		cfg.DaemonMode = true
	}
	if *logFile != "" {
		cfg.LogFile = *logFile
	}
	if *interval > 0 {
		cfg.Interval = *interval
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	if *topN > 0 {
		cfg.TopN = *topN
	}
	if *maxIPs > 0 {
		cfg.MaxIPs = *maxIPs
	}
	if *ipv4Only {
		// IPv6 已移除，此标志保留向后兼容
	}
	if *ipSelectMode != "" {
		cfg.IPSelectMode = strings.ToLower(*ipSelectMode)
	}

	// 重新验证配置（命令行参数覆盖后）
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置验证失败: %v\n", err)
		os.Exit(1)
	}

	// P2-10: 直连模式 — 启动早期清除代理环境变量
	engine.ApplyDirectMode(cfg)

	if _, err := os.Stat(*configFile); os.IsNotExist(err) {
		if err := config.SaveConfig(cfg, *configFile); err != nil {
			fmt.Fprintf(os.Stderr, "保存配置失败: %v\n", err)
		} else {
			fmt.Printf("已创建默认配置文件: %s\n", *configFile)
		}
	}

	logger, err = log.NewLogger(cfg.LogFile, log.LevelInfo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()

	// 启动时清理上次更新遗留的临时文件(Windows .old 二进制等)
	// update_state.json 由 Installer 在 Windows 替换二进制时写入
	// Linux 不写 update_state.json,该函数为 no-op(仅检查文件是否存在)
	updater.CleanupOnStartup(logger)

	// 初始化资源清理器
	cleaner = cleanup.New(cfg, logger)

	logger.Info("STARTUP", "cf-speedtest v%s 启动", version)
	logger.Info("CONFIG", "配置文件: %s", *configFile)
	logger.Info("CONFIG", "日志文件: %s", logger.GetLogPath())
	logger.Info("CONFIG", "IP 选择模式: %s", cfg.IPSelectMode)
	if cfg.IPSelectMode == config.IPSelectModeManual {
		logger.Info("CONFIG", "手动选择国家/地区: %v", cfg.IPSelectCountries)
		fmt.Printf("[INFO] IP 选择模式: 手动 (%v)\n", cfg.IPSelectCountries)
	} else {
		fmt.Printf("[INFO] IP 选择模式: 自动\n")
	}

	// 端口配置日志
	logger.Info("CONFIG", "TCP 测试端口: %v（共 %d 个）", cfg.TCPPingPorts, len(cfg.TCPPingPorts))
	fmt.Printf("[INFO] TCP 测试端口: %v（共 %d 个）\n", cfg.TCPPingPorts, len(cfg.TCPPingPorts))
	for _, port := range cfg.TCPPingPorts {
		if port <= 1023 {
			logger.Warn("CONFIG", "端口 %d 为系统保留端口（1-1023），请确保不会与其他服务冲突", port)
		}
	}

	db, err := repository.NewDB(cfg.DBPath)
	if err != nil {
		logger.Error("DB", "初始化数据库失败: %v", err)
		fmt.Fprintf(os.Stderr, "初始化数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	logger.Info("DB", "数据库初始化成功，文件: %s", cfg.DBPath)
	fmt.Printf("[INFO] 数据库初始化成功\n")

	// 初始化 Resolver：xdb 快速初筛 + 纠错覆盖层 + cdn-cgi/trace 精准验证
	var traceVerifier *geo.TraceVerifier
	if cfg.TraceVerifyEnable {
		traceVerifier = geo.NewTraceVerifier(cfg.TraceEndpoint, cfg.TraceHTTPTimeout, cfg.TraceConnectTimeout)
	}
	resolver, err := geo.NewResolverWithCorrections(cfg.IPDBPath, cfg.GeoCorrectionsPath, traceVerifier)
	if err != nil {
		logger.Warn("GEO", "初始化 IP 归属地解析器失败: %v", err)
		fmt.Printf("[WARN] 初始化 IP 归属地解析器失败: %v\n", err)
	} else if resolver.IsEnabled() {
		extras := ""
		if resolver.CorrectionsCount() > 0 {
			extras = fmt.Sprintf(", 纠错记录: %d 条", resolver.CorrectionsCount())
		}
		if cfg.TraceVerifyEnable {
			extras += ", cdn-cgi/trace 验证已启用"
		}
		logger.Info("GEO", "IP 归属地解析器已启用，数据库: %s%s", cfg.IPDBPath, extras)
		fmt.Printf("[INFO] IP 归属地解析器已启用%s\n", extras)
	} else {
		logger.Warn("GEO", "IP 归属地解析器未启用（数据库文件不存在或加载失败），将以降级模式运行")
		fmt.Printf("[WARN] IP 归属地解析器未启用，将以降级模式运行\n")
	}
	defer resolver.Close()

	// 开机时主动检测本机 ISP
	logger.Info("ISP", "开始检测本机运营商...")
	localISP, localCountry, publicIP := resolver.DetectLocalISP()
	if publicIP != "" {
		logger.Info("ISP", "本机公网IP: %s, 运营商: %s, 国家: %s", publicIP, localISP, localCountry)
		fmt.Printf("[INFO] 本机公网IP: %s, 运营商: %s, 国家: %s\n", publicIP, localISP, localCountry)
	} else {
		logger.Warn("ISP", "本机公网IP检测失败，运营商标记为 Unknown")
		fmt.Printf("[WARN] 本机公网IP检测失败，运营商标记为 Unknown\n")
	}

	if cfg.DaemonMode {
		// systemd 已负责后台化进程（Type=simple），在此环境下跳过传统 daemonize
		// 检测方式: systemd 为每个服务设置 INVOCATION_ID 环境变量
		if os.Getenv("INVOCATION_ID") != "" {
			logger.Info("DAEMON", "检测到 systemd 环境，跳过 daemonize（由 systemd 管理进程生命周期）")
			fmt.Println("[INFO] systemd 环境检测到，由 systemd 管理进程生命周期")
		} else if os.Getenv("CF_DAEMONIZED") == "1" {
			// 已通过 daemonize() 脱离终端的子进程，无需再次 daemonize
			logger.Info("DAEMON", "已处于后台模式（CF_DAEMONIZED=1），跳过 daemonize")
		} else if err := daemonize(cfg.LogFile); err != nil {
			logger.Error("DAEMON", "后台运行启动失败: %v", err)
			fmt.Fprintf(os.Stderr, "后台运行启动失败: %v\n", err)
			os.Exit(1)
		} else {
			logger.Info("DAEMON", "程序已后台运行，PID: %d", os.Getpid())
			fmt.Printf("程序已后台运行，PID: %d\n", os.Getpid())
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 创建后台模式控制器
	daemonCtrl := web.NewDaemonControl(cfg.DaemonMode)

	// 初始化 CIDR 权重统计器（基于历史纠错命中率动态调整采样权重）
	if cfg.CIDRStatsPath != "" {
		cidrStats = collector.NewCIDRStats(cfg.CIDRStatsPath)
		logger.Info("CIDR", "CIDR 权重统计已启用，统计文件: %s", cfg.CIDRStatsPath)
		fmt.Printf("[INFO] CIDR 权重统计已启用\n")
	}

	// 启动 Web Dashboard（如启用）
	if cfg.WebEnable {
		// 创建版本检查器 + 更新管理器(Phase 1:仅检查;Phase 2+:下载/校验/安装/重启)
		var checker *updater.Checker
		var mgr *updater.Manager
		if cfg.UpdateCheckEnable {
			if cfg.UpdateCheckURL == "" {
				logger.Warn("UPDATE", "update_check_enable=true 但 update_check_url 为空,跳过更新检查器初始化")
			} else if version == "" {
				logger.Warn("UPDATE", "当前版本号为空,跳过更新检查器初始化")
			} else {
				checker = updater.NewChecker(cfg.UpdateCheckURL, version, logger)
				dl := updater.NewDownloader(cfg.UpdateTempDir, logger)
				mgr = updater.NewManager(checker, dl, logger)
				// 启动后台版本检查 goroutine:启动 30s 后首次检查,之后按 ticker 间隔重复
				go startUpdateChecker(checker, cfg.UpdateCheckInterval, logger)
			}
		} else {
			logger.Info("UPDATE", "自动更新检查已禁用(update_check_enable=false)")
		}

		webSrv := web.NewServer(web.Deps{
			Cfg:       cfg,
			CfgPath:   *configFile,
			DB:        db,
			Resolver:  resolver,
			Logger:    logger,
			Version:   version,
			Daemon:    daemonCtrl,
			CIDRStats: cidrStats,
			Checker:   checker,
			Manager:   mgr,
		})
		// 注入重启器(Phase 3 中 Manager 安装完成后调用 webSrv.Restart)
		if mgr != nil {
			mgr.SetRestarter(webSrv)
		}
		go func() {
			if err := webSrv.Start(); err != nil {
				logger.Error("WEB", "Web 服务退出: %v", err)
				fmt.Fprintf(os.Stderr, "Web 服务退出: %v\n", err)
			}
		}()
	}

	// 运行模式: daemon 或 web 启用时进入长驻调度；否则执行一次后退出
	if cfg.DaemonMode || cfg.WebEnable {
		runDaemon(cfg, resolver, db, sigChan, cfg.DaemonMode, daemonCtrl)
	} else {
		runBenchmark(cfg, resolver, db)
	}
}

// runDaemon 后台模式调度器
// 支持两种独立的定时任务：
//   - 数据采集（collect_time）: 每天定时执行一次完整采集
//   - 自动推送（push_interval）: 每 N 小时重测并推送
//
// immediate 控制是否在启动时立即执行一次采集（daemon 模式默认执行；纯 web 模式跳过）
func runDaemon(cfg *config.Config, resolver *geo.Resolver, db *repository.DB, sigChan chan os.Signal, immediate bool, dc *web.DaemonControl) {
	// 启动时立即执行一次采集（仅 daemon 模式；web 模式由用户手动触发或等待定时任务）
	if immediate {
		logger.Info("DAEMON", "启动时执行首次数据采集...")
		fmt.Println("[INFO] 启动时执行首次数据采集...")
		runBenchmark(cfg, resolver, db)
	} else {
		logger.Info("DAEMON", "Web 模式启动，跳过启动时采集，等待定时任务或手动触发")
		fmt.Println("[INFO] Web 模式启动，跳过启动时采集，等待定时任务或手动触发")
	}

	var collectTimer *time.Timer
	var pushTimer *time.Timer

	// 启动采集定时器
	startCollectTimer := func() {
		if cfg.CollectTime != "" {
			nextCollect := nextTimeMulti(cfg.CollectTime)
			collectTimer = time.NewTimer(time.Until(nextCollect))
			dc.SetNextCollect(nextCollect)
			logger.Info("DAEMON", "定时采集已激活，配置: %s，下次: %s", cfg.CollectTime, nextCollect.Format("2006-01-02 15:04:05"))
		} else {
			d := time.Duration(cfg.Interval) * time.Minute
			collectTimer = time.NewTimer(d)
			dc.SetNextCollect(time.Now().Add(d))
			logger.Info("DAEMON", "定时采集已激活，interval=%d 分钟", cfg.Interval)
		}
	}
	// 启动推送定时器
	startPushTimer := func() {
		if cfg.PushInterval > 0 {
			d := time.Duration(cfg.PushInterval) * time.Hour
			pushTimer = time.NewTimer(d)
			nextPush := time.Now().Add(d)
			dc.SetNextPush(nextPush)
			logger.Info("DAEMON", "定时推送已激活，下次: %s", nextPush.Format("2006-01-02 15:04:05"))
		}
	}

	if dc.IsActive() {
		startCollectTimer()
		startPushTimer()
	} else {
		logger.Info("DAEMON", "后台模式未启用，定时任务已停止")
		fmt.Println("[INFO] 后台模式未启用，定时任务已停止")
	}

	for {
		select {
		case <-pushChan(collectTimer):
			if dc.IsActive() {
				logger.Info("DAEMON", "定时数据采集触发")
				fmt.Println("[INFO] 定时数据采集触发")
				collectStart := time.Now()
				runBenchmark(cfg, resolver, db)
				pushAfterBenchmark(cfg, resolver, db, collectStart)
			}
			if cfg.CollectTime != "" {
				next := nextTimeMulti(cfg.CollectTime)
				collectTimer.Reset(time.Until(next))
				dc.SetNextCollect(next)
				logger.Info("DAEMON", "下次数据采集时间: %s", next.Format("2006-01-02 15:04:05"))
			} else {
				d := time.Duration(cfg.Interval) * time.Minute
				collectTimer.Reset(d)
				dc.SetNextCollect(time.Now().Add(d))
			}

		case <-pushChan(pushTimer):
			if dc.IsActive() {
				logger.Info("DAEMON", "定时自动推送触发")
				fmt.Println("[INFO] 定时自动推送触发")
				if err := rerunAndPush(cfg, resolver, db, time.Time{}); err != nil {
					logger.Error("PUSH", "定时推送失败: %v", err)
					fmt.Fprintf(os.Stderr, "定时推送失败: %v\n", err)
				}
			}
			if cfg.PushInterval > 0 {
				d := time.Duration(cfg.PushInterval) * time.Hour
				pushTimer.Reset(d)
				nextPush := time.Now().Add(d)
				dc.SetNextPush(nextPush)
				logger.Info("DAEMON", "下次自动推送时间: %s", nextPush.Format("2006-01-02 15:04:05"))
			}

		case newState := <-dc.ToggleChan():
			if newState {
				logger.Info("DAEMON", "后台模式已激活，启动定时任务")
				fmt.Println("[INFO] 后台模式已激活，定时任务已启动")
				startCollectTimer()
				startPushTimer()
			} else {
				logger.Info("DAEMON", "后台模式已停止，定时任务暂停")
				fmt.Println("[INFO] 后台模式已停止，定时任务已暂停")
				if collectTimer != nil {
					collectTimer.Stop()
				}
				if pushTimer != nil {
					pushTimer.Stop()
				}
				dc.SetNextCollect(time.Time{})
				dc.SetNextPush(time.Time{})
			}

		case sig := <-sigChan:
			logger.Info("SHUTDOWN", "收到信号 %v，程序退出", sig)
			fmt.Printf("\n收到信号 %v，程序退出\n", sig)
			if collectTimer != nil {
				collectTimer.Stop()
			}
			if pushTimer != nil {
				pushTimer.Stop()
			}
			return
		}
	}
}

// pushChan 辅助函数：将 timer 转换为 channel（nil 时返回 nil channel 永久阻塞）
func pushChan(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// nextTime 计算下一个采集时间点（今天或明天的指定时刻）
func nextTime(hhmm string) time.Time {
	now := time.Now()
	t, _ := time.Parse("15:04", hhmm)
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// nextTimeMulti 支持逗号分隔的多个时间点（如 "06:00,12:00,18:00"），返回最近的下一个触发时间
func nextTimeMulti(hhmmList string) time.Time {
	now := time.Now()
	var earliest time.Time
	for _, hhmm := range strings.Split(hhmmList, ",") {
		hhmm = strings.TrimSpace(hhmm)
		if hhmm == "" {
			continue
		}
		t, _ := time.Parse("15:04", hhmm)
		next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	return earliest
}

func runBenchmark(cfg *config.Config, resolver *geo.Resolver, db *repository.DB) {
	startTime := time.Now()
	logger.Info("BENCHMARK", "开始测速任务")

	// 资源清理：任务结束后执行
	var baseline *cleanup.ResourceSnapshot
	if cleaner != nil {
		baseline = cleanup.Snapshot()
		defer func() {
			result := cleaner.Cleanup(config.JobTypeBenchmark, baseline, db)
			logger.Info("CLEANUP", "测速任务清理: 内存释放 %.1f%%, 临时文件 %d, 进程 %d, 耗时 %s",
				result.MemoryRatio*100, result.TempFilesDeleted, result.ProcessesKilled, result.Duration.Round(time.Millisecond))
		}()
	}

	logger.Info("FETCH", "开始获取 IP 段（官方地址: %v, 数量: %d）...", cfg.IPv4Enabled, cfg.IPv4Count)
	fmt.Printf("[INFO] 开始获取 IP（官方: %v, 数量: %d）...\n", cfg.IPv4Enabled, cfg.IPv4Count)
	fetcher := collector.NewFetcherWithStats(cfg, cidrStats)
	fetchRes, err := fetcher.FetchCategorized(func(p collector.FetchProgress) {
		fmt.Printf("  拉取进度[%s] %s: 本URL %d 个, 累计 官方%d+自定义%d=%d\n",
			p.Source, p.URL, p.Count, p.Official, p.Custom, p.Total)
	})
	if err != nil {
		logger.Error("FETCH", "获取 IP 段失败: %v", err)
		fmt.Fprintf(os.Stderr, "获取 IP 段失败: %v\n", err)
		return
	}

	// 数据整合: 合并官方与自定义地址的 IP
	allTasks := make([]model.Task, 0, len(fetchRes.OfficialTasks)+len(fetchRes.CustomTasks))
	allTasks = append(allTasks, fetchRes.OfficialTasks...)
	allTasks = append(allTasks, fetchRes.CustomTasks...)

	if len(allTasks) == 0 {
		logger.Warn("FETCH", "未获取到任何 IP 地址")
		fmt.Println("[WARN] 未获取到任何 IP 地址")
		return
	}
	logger.Info("FETCH", "成功获取 %d 个 IP 地址（官方 %d + 自定义 %d）",
		len(allTasks), len(fetchRes.OfficialTasks), len(fetchRes.CustomTasks))
	fmt.Printf("[INFO] 共获取 %d 个 IP（官方 %d + 自定义 %d）\n",
		len(allTasks), len(fetchRes.OfficialTasks), len(fetchRes.CustomTasks))

	// 端口预过滤: 清除数据库中不在用户配置端口列表中的旧记录
	if len(cfg.TCPPingPorts) > 0 {
		deleted, err := db.DeleteByPortsNotIn(cfg.TCPPingPorts)
		if err != nil {
			logger.Error("DB", "端口预过滤失败: %v", err)
			fmt.Fprintf(os.Stderr, "[ERROR] 端口预过滤失败: %v\n", err)
			return
		}
		if deleted > 0 {
			logger.Info("DB", "端口预过滤: 清除非配置端口旧记录 %d 条", deleted)
			fmt.Printf("[INFO] 端口预过滤: 清除非配置端口旧记录 %d 条\n", deleted)
		}
		// 验证: 确认数据库中不再有非配置端口记录
		remaining, err := db.CountByPortsNotIn(cfg.TCPPingPorts)
		if err != nil {
			logger.Error("DB", "端口验证查询失败: %v", err)
		} else if remaining > 0 {
			logger.Warn("DB", "端口验证: 仍有 %d 条非配置端口记录，再次清理", remaining)
			db.DeleteByPortsNotIn(cfg.TCPPingPorts)
		} else {
			logger.Info("DB", "端口验证通过: 数据库仅含配置端口 %v 的记录", cfg.TCPPingPorts)
			fmt.Printf("[INFO] 端口验证通过: 数据库仅含配置端口 %v 的记录\n", cfg.TCPPingPorts)
		}
	}

	logger.Info("INCREMENTAL", "查询数据库中未过期的 IP...")
	validIPs, err := db.GetValidIPs(cfg.IPExpireTime)
	if err != nil {
		logger.Error("DB", "查询有效 IP 失败: %v", err)
		fmt.Fprintf(os.Stderr, "查询有效 IP 失败: %v\n", err)
		return
	}

	var tasks []model.Task
	for _, task := range allTasks {
		key := fmt.Sprintf("%s:%d", task.IP, task.Port)
		if !validIPs[key] {
			tasks = append(tasks, task)
		}
	}

	logger.Info("INCREMENTAL", "全量 IP: %d, 需增量测速 IP: %d, 跳过已缓存 IP: %d", len(allTasks), len(tasks), len(allTasks)-len(tasks))
	fmt.Printf("[INFO] 全量 IP: %d, 需增量测速 IP: %d, 跳过已缓存 IP: %d\n", len(allTasks), len(tasks), len(allTasks)-len(tasks))

	// TCP 握手预筛选: 并发测试连通性，清除不可达 IP
	if len(tasks) > 0 {
		preTimeout := 3 * time.Second
		if cfg.TCPPingTimeout > 0 {
			preTimeout = cfg.TCPPingTimeout
		}
		logger.Info("PREFILTER", "开始TCP握手预筛选 %d 个IP (并发: %d, 超时: %s)", len(tasks), cfg.Concurrency, preTimeout)
		fmt.Printf("[INFO] 开始TCP握手预筛选 %d 个IP...\n", len(tasks))
		preResults, preStats := engine.PreFilterTCP(tasks, cfg.Concurrency, preTimeout)
		logger.Info("PREFILTER", "预筛选完成: 成功 %d, 失败 %d, 耗时 %s, 平均延迟 %s",
			preStats.Success, preStats.Failed, preStats.Duration.Round(time.Millisecond), preStats.AvgLatency.Round(time.Millisecond))
		fmt.Printf("[INFO] TCP握手预筛选: %d → %d (失败 %d, 耗时 %s)\n",
			preStats.Total, preStats.Success, preStats.Failed, preStats.Duration.Round(time.Millisecond))

		// 仅保留握手成功的 IP
		filteredTasks := engine.FilterReachable(tasks, preResults)
		if len(filteredTasks) == 0 {
			logger.Warn("PREFILTER", "所有IP握手失败，无可用IP")
			fmt.Println("[WARN] 所有IP握手失败，无可用IP进入测速")
			return
		}
		tasks = filteredTasks
		logger.Info("PREFILTER", "预筛选后剩余 %d 个IP进入测速", len(tasks))
		fmt.Printf("[INFO] 预筛选后剩余 %d 个IP进入测速\n", len(tasks))
	}

	// 官方地址补全国家代码：xdb快速初筛（不做trace验证，仅用于初步过滤）
	engine.FillCountryCodes(tasks, resolver, false)

	// 手动模式: 按用户指定国家筛选（测速前预过滤，避免对不符合条件的IP浪费测速资源）
	if cfg.IPSelectMode == config.IPSelectModeManual && len(cfg.IPSelectCountries) > 0 {
		beforeFilter := len(tasks)
		tasks = engine.FilterTasksByCountries(tasks, cfg.IPSelectCountries)
		logger.Info("FILTER", "手动模式国家筛选[%v]: %d → %d", cfg.IPSelectCountries, beforeFilter, len(tasks))
		fmt.Printf("[INFO] 国家筛选[%v]: %d → %d\n", cfg.IPSelectCountries, beforeFilter, len(tasks))
		if len(tasks) == 0 {
			logger.Warn("FILTER", "国家筛选后无可用IP")
			fmt.Println("[WARN] 国家筛选后无可用IP")
			return
		}

		// 纠错验证：对筛选后的IP做cdn-cgi/trace精准验证，纠正xdb误判
		// 验证失败的IP（trace超时/错误）直接移除，不进入测速
		// 纠错后再次过滤：不在用户选择国家中的IP直接删除
		if cfg.TraceVerifyEnable && resolver != nil && resolver.IsEnabled() {
			beforeVerify := len(tasks)
			verifyConcurrency := cfg.TraceVerifyConcurrency
			if verifyConcurrency <= 0 {
				verifyConcurrency = 10
			}
			var corrected, failed int
			tasks, corrected, failed = geo.VerifyTasksCountryCodes(tasks, resolver, verifyConcurrency)
			logger.Info("GEO", "测速前纠错验证: %d 个IP, 修正 %d 条, 失败(移除) %d 条, 剩余 %d 条",
				beforeVerify, corrected, failed, len(tasks))
			fmt.Printf("[INFO] 测速前纠错验证: 修正 %d 条, 移除 %d 条, 剩余 %d 条\n",
				corrected, failed, len(tasks))

			// 纠错后重新过滤（纠正后国家代码可能不再符合用户选择）
			afterVerify := len(tasks)
			tasks = engine.FilterTasksByCountries(tasks, cfg.IPSelectCountries)
			removed := afterVerify - len(tasks)
			if removed > 0 {
				logger.Info("FILTER", "纠错后重新筛选: 移除 %d 条不属于目标国家的IP", removed)
				fmt.Printf("[INFO] 纠错后重新筛选: 移除 %d 条不属于目标国家的IP\n", removed)
			}
			if len(tasks) == 0 {
				logger.Warn("FILTER", "纠错验证后无可用IP")
				fmt.Println("[WARN] 纠错验证后无可用IP")
				return
			}
			// 更新 CIDR 权重统计：记录各 CIDR 通过验证+国家筛选的 IP 数
			if cidrStats != nil {
				passedPerCIDR := make(map[string]int)
				for _, t := range tasks {
					if t.SourceCIDR != "" {
						passedPerCIDR[t.SourceCIDR]++
					}
				}
				for cidr, cnt := range passedPerCIDR {
					cidrStats.RecordPassed(cidr, cnt)
				}
				if err := cidrStats.Save(); err != nil {
					logger.Warn("CIDR", "CIDR 统计保存失败: %v", err)
				} else {
					logger.Info("CIDR", "权重统计已更新: %d 个 CIDR", len(passedPerCIDR))
					fmt.Printf("[INFO] CIDR 权重已更新: %d 个 CIDR\n", len(passedPerCIDR))
				}
			}
		}
	}

	if len(tasks) == 0 {
		logger.Info("INCREMENTAL", "所有 IP 均在有效期内，从数据库读取结果...")
		fmt.Println("[INFO] 所有 IP 均在有效期内，从数据库读取结果...")

		topResults, err := db.GetTopResultsByPorts(cfg.TopN, cfg.TCPPingPorts)
		if err != nil {
			logger.Error("DB", "读取数据库结果失败: %v", err)
			fmt.Fprintf(os.Stderr, "读取数据库结果失败: %v\n", err)
			return
		}

		outputResults(topResults, cfg, resolver, logger)
		duration := time.Since(startTime)
		logger.Info("BENCHMARK", "增量测速任务完成（从缓存读取），耗时: %.2f 秒", duration.Seconds())
		fmt.Printf("[INFO] 增量测速任务完成（从缓存读取），耗时: %.2f 秒\n", duration.Seconds())
		return
	}

	logger.Info("BENCHMARK", "开始测速 (并发: %d)", cfg.Concurrency)
	bench := engine.NewBenchmarkEngine(cfg, resolver)
	resultCh := bench.Run(tasks)

	var results []model.IPResult
	completed := 0
	total := len(tasks)
	for r := range resultCh {
		results = append(results, r)
		completed++
		if completed%500 == 0 {
			logger.Info("BENCHMARK", "测速进度: %d/%d (%.1f%%)", completed, total, float64(completed)/float64(total)*100)
			fmt.Printf("  进度: %d/%d\n", completed, total)
		}
	}
	logger.Info("BENCHMARK", "测速完成，共 %d 个有效结果", len(results))
	fmt.Printf("  测速完成，共 %d 个结果\n", len(results))

	// 先评分再写入，确保数据库中存储的 Score 是最新值
	scoredResults := scorer.ScoreAllResults(results, cfg)
	if scoredResults != nil && len(scoredResults) > 0 {
		logger.Info("SCORER", "全量评分完成，共 %d 个有效结果", len(scoredResults))

		// 按评分阈值过滤
		var dbResults []model.IPResult
		filteredCount := 0
		zeroSpeedCount := 0
		hardRuleFiltered := 0
		for _, r := range scoredResults {
			// 数据清洗: 移除下载速度为0的IP（下载失败或无带宽）
			if r.DownloadSpeed <= 0 {
				zeroSpeedCount++
				continue
			}
			// 硬性规则验证: TCP延迟/丢包率/HTTP延迟/下载带宽
			if !cfg.PassesHardRules(r) {
				hardRuleFiltered++
				continue
			}
			if r.Score >= cfg.MinScoreThreshold {
				dbResults = append(dbResults, r)
			} else {
				filteredCount++
			}
		}

		if zeroSpeedCount > 0 {
			logger.Info("DB", "数据清洗: 移除 %d 条下载速度为0的IP", zeroSpeedCount)
			fmt.Printf("[INFO] 数据清洗: 移除 %d 条下载速度为0的IP\n", zeroSpeedCount)
		}
		if hardRuleFiltered > 0 {
			logger.Info("DB", "硬性规则过滤: %d 条IP未通过入库规则（TCP延迟>%dms/丢包>%.1f/HTTP延迟>%dms/带宽<%.1fMbps）",
				hardRuleFiltered, cfg.RuleMaxTCPLatency, cfg.RuleMaxLossRate, cfg.RuleMaxHTTPLatency, cfg.RuleMinDownloadMbps)
			fmt.Printf("[INFO] 硬性规则过滤: %d 条IP未通过入库规则\n", hardRuleFiltered)
		}
		if cfg.MinScoreThreshold > 0 {
			logger.Info("DB", "评分阈值过滤: %.4f，通过 %d 条，过滤 %d 条（低于阈值）",
				cfg.MinScoreThreshold, len(dbResults), filteredCount)
			fmt.Printf("[INFO] 评分阈值 %.4f 过滤: 通过 %d 条，过滤 %d 条\n",
				cfg.MinScoreThreshold, len(dbResults), filteredCount)
		}

		// 按综合评分降序排序
		sort.Slice(dbResults, func(i, j int) bool {
			return dbResults[i].Score > dbResults[j].Score
		})

		logger.Info("DB", "批量写入数据库...")
		if err := db.BatchUpsert(dbResults); err != nil {
			logger.Error("DB", "批量写入数据库失败: %v", err)
			fmt.Fprintf(os.Stderr, "批量写入数据库失败: %v\n", err)
		} else {
			logger.Info("DB", "成功写入 %d 条记录", len(dbResults))
			fmt.Printf("[INFO] 成功写入 %d 条记录\n", len(dbResults))
		}

		// 末位淘汰: 超过 MaxDBSize 时按评分从低到高淘汰
		if cfg.MaxDBSize > 0 {
			currentCount, err := db.Count()
			if err != nil {
				logger.Error("DB", "查询数据库记录数失败: %v", err)
			} else {
				logger.Info("DB", "当前数据库记录数: %d, 上限: %d", currentCount, cfg.MaxDBSize)
				if currentCount > int64(cfg.MaxDBSize) {
					deleted, err := db.TrimToMaxSize(cfg.MaxDBSize)
					if err != nil {
						logger.Error("DB", "末位淘汰失败: %v", err)
						fmt.Fprintf(os.Stderr, "末位淘汰失败: %v\n", err)
					} else {
						logger.Info("DB", "末位淘汰完成，删除 %d 条低分记录，剩余 %d 条", deleted, cfg.MaxDBSize)
						fmt.Printf("[INFO] 末位淘汰: 删除 %d 条低分记录，数据库剩余 %d 条\n", deleted, cfg.MaxDBSize)
					}
				}
			}
		}
	} else {
		logger.Warn("SCORER", "没有有效的测速结果可供评分")
	}

	logger.Info("DB", "清理过期数据（保留 %d 天）...", cfg.DataRetention)
	deleted, err := db.CleanupOldData(cfg.DataRetention)
	if err != nil {
		logger.Error("DB", "清理过期数据失败: %v", err)
	} else {
		logger.Info("DB", "清理完成，删除 %d 条过期记录", deleted)
		fmt.Printf("[INFO] 清理完成，删除 %d 条过期记录\n", deleted)
	}

	logger.Info("DB", "从数据库读取最新 Top 结果...")
	topResults, err := db.GetTopResultsByPorts(cfg.TopN, cfg.TCPPingPorts)
	if err != nil {
		logger.Error("DB", "读取数据库结果失败: %v", err)
		fmt.Fprintf(os.Stderr, "读取数据库结果失败: %v\n", err)
		return
	}

	outputResults(topResults, cfg, resolver, logger)

	// 非后台模式: 如果命令行指定了 rerun_all/push_only，则执行推送
	// 后台模式下，推送由 runDaemon 的定时器独立调度
	if !cfg.DaemonMode && (*rerunAll || *pushOnly) {
		if err := rerunAndPush(cfg, resolver, db, time.Time{}); err != nil {
			logger.Error("PUSH", "全量重测推送失败: %v", err)
			fmt.Fprintf(os.Stderr, "全量重测推送失败: %v\n", err)
		}
	}

	duration := time.Since(startTime)
	logger.Info("BENCHMARK", "测速任务完成，耗时: %.2f 秒", duration.Seconds())
	fmt.Printf("[INFO] 测速任务完成，耗时: %.2f 秒\n", duration.Seconds())
}

// rerunAndPush 对数据库中所有 IP 重新测速、排序，并推送到 Cloudflare / GitHub
// 根据需求：推送时从数据库读取所有IP，重新测速评分，按评分从高到低排序，取前50个写入IP.txt
// skipAfter 非零时跳过该时间之后更新的IP（刚采集的），仅重测历史IP
func rerunAndPush(cfg *config.Config, resolver *geo.Resolver, db *repository.DB, skipAfter time.Time) error {
	logger.Info("PUSH", "开始推送流程：重测数据库中所有 IP")
	fmt.Println("\n=== 推送流程：重测 + 排序 + 写入 IP.txt ===")

	// 资源清理：任务结束后执行
	var baseline *cleanup.ResourceSnapshot
	if cleaner != nil {
		baseline = cleanup.Snapshot()
		defer func() {
			result := cleaner.Cleanup(config.JobTypePush, baseline, db)
			logger.Info("CLEANUP", "推送任务清理: 内存释放 %.1f%%, 临时文件 %d, 进程 %d, 耗时 %s",
				result.MemoryRatio*100, result.TempFilesDeleted, result.ProcessesKilled, result.Duration.Round(time.Millisecond))
		}()
	}

	// 1. 获取数据库中所有 IP（仅限用户配置端口）
	// skipAfter 非零时仅获取历史IP（跳过刚采集的），减少重复重测
	var allTasks []model.Task
	var err error
	if !skipAfter.IsZero() {
		if len(cfg.TCPPingPorts) > 0 {
			allTasks, err = db.GetIPsByPortsBefore(cfg.TCPPingPorts, skipAfter)
		} else {
			allTasks, err = db.GetAllIPsBefore(skipAfter)
		}
		logger.Info("PUSH", "采集后推送模式: 仅重测 %s 之前更新的历史IP", skipAfter.Format("15:04:05"))
	} else {
		if len(cfg.TCPPingPorts) > 0 {
			allTasks, err = db.GetIPsByPorts(cfg.TCPPingPorts)
		} else {
			allTasks, err = db.GetAllIPs()
		}
	}
	if err != nil {
		return fmt.Errorf("获取数据库 IP 失败: %w", err)
	}

	if len(allTasks) == 0 {
		logger.Warn("PUSH", "数据库中没有匹配端口 %v 的 IP 可供重测", cfg.TCPPingPorts)
		fmt.Printf("[WARN] 数据库中没有匹配端口 %v 的 IP 可供重测\n", cfg.TCPPingPorts)
		return nil
	}

	logger.Info("PUSH", "准备重测 %d 个数据库 IP", len(allTasks))
	fmt.Printf("[INFO] 准备重测 %d 个数据库 IP\n", len(allTasks))

	// 2. 全量重测
	logger.Info("PUSH", "开始全量重测 (并发: %d)", cfg.Concurrency)
	fmt.Printf("[INFO] 开始全量重测 (并发: %d)\n", cfg.Concurrency)

	bench := engine.NewBenchmarkEngine(cfg, resolver)
	resultCh := bench.Run(allTasks)

	var reResults []model.IPResult
	completed := 0
	total := len(allTasks)
	for r := range resultCh {
		reResults = append(reResults, r)
		completed++
		if completed%500 == 0 {
			logger.Info("PUSH", "重测进度: %d/%d (%.1f%%)", completed, total, float64(completed)/float64(total)*100)
			fmt.Printf("  重测进度: %d/%d\n", completed, total)
		}
	}
	logger.Info("PUSH", "重测完成，共 %d 个有效结果", len(reResults))
	fmt.Printf("  重测完成，共 %d 个结果\n", len(reResults))

	// 3. 100分制评分 + 排序
	scoredResults := scorer.ScoreAllResults(reResults, cfg)
	if scoredResults == nil || len(scoredResults) == 0 {
		logger.Warn("PUSH", "重测无有效结果")
		fmt.Println("[WARN] 重测无有效结果")
		return nil
	}

	// 按评分降序排序
	sort.Slice(scoredResults, func(i, j int) bool {
		return scoredResults[i].Score > scoredResults[j].Score
	})

	// 4. 硬性规则过滤 + 更新数据库（重测结果）
	var pushValidResults []model.IPResult
	pushHardFiltered := 0
	for _, r := range scoredResults {
		if !cfg.PassesHardRules(r) {
			pushHardFiltered++
			continue
		}
		pushValidResults = append(pushValidResults, r)
	}
	if pushHardFiltered > 0 {
		logger.Info("PUSH", "硬性规则过滤: %d 条IP未通过入库规则", pushHardFiltered)
		fmt.Printf("[INFO] 硬性规则过滤: %d 条IP未通过入库规则\n", pushHardFiltered)
	}
	if len(pushValidResults) == 0 {
		logger.Warn("PUSH", "硬性规则过滤后无有效结果")
		return nil
	}
	scoredResults = pushValidResults
	logger.Info("PUSH", "更新数据库中的重测结果...")
	if err := db.BatchUpsert(scoredResults); err != nil {
		logger.Error("PUSH", "更新数据库失败: %v", err)
		return fmt.Errorf("更新数据库失败: %w", err)
	}

	// 末位淘汰
	if cfg.MaxDBSize > 0 {
		currentCount, _ := db.Count()
		if currentCount > int64(cfg.MaxDBSize) {
			deleted, _ := db.TrimToMaxSize(cfg.MaxDBSize)
			logger.Info("PUSH", "末位淘汰: 删除 %d 条低分记录", deleted)
			fmt.Printf("[INFO] 末位淘汰: 删除 %d 条低分记录\n", deleted)
		}
	}

	// 5. 取前50个结果写入 IP.txt（按需求固定50个）
	topN := 50
	if cfg.CFPushCount > topN {
		topN = cfg.CFPushCount
	}
	if cfg.GithubPushCount > topN {
		topN = cfg.GithubPushCount
	}
	if topN > len(scoredResults) {
		topN = len(scoredResults)
	}
	topResults := scoredResults[:topN]

	logger.Info("PUSH", "取前 %d 个结果写入 IP.txt", topN)
	fmt.Printf("[INFO] 取前 %d 个结果写入 IP.txt\n", topN)

	// 6. 生成 IP.txt
	ipTxtPath := cfg.GithubFilePath
	if ipTxtPath == "" {
		ipTxtPath = "IP.txt"
	}
	if err := output.ExportIPTxt(topResults, ipTxtPath, topN); err != nil {
		logger.Error("PUSH", "生成 IP.txt 失败: %v", err)
		fmt.Fprintf(os.Stderr, "生成 IP.txt 失败: %v\n", err)
	} else {
		logger.Info("PUSH", "IP.txt 已生成: %s (前 %d 个)", ipTxtPath, topN)
		fmt.Printf("[INFO] IP.txt 已生成: %s (前 %d 个)\n", ipTxtPath, topN)
	}

	// 7. 推送到 Cloudflare DNS（仅推送端口443的IP）
	if cfg.CFPushCount > 0 && cfg.CFAPIKey != "" && cfg.CFZoneID != "" && cfg.CFDNSName != "" {
		// 筛选端口为 443 的 IP（Cloudflare DNS 仅支持标准 HTTPS 端口）
		var cfCandidates []model.IPResult
		for _, r := range topResults {
			if r.Port == 443 {
				cfCandidates = append(cfCandidates, r)
			}
		}
		logger.Info("PUSH", "Cloudflare推送端口筛选: %d → %d (仅443)", len(topResults), len(cfCandidates))
		fmt.Printf("[INFO] Cloudflare推送端口筛选: %d → %d (仅端口443)\n", len(topResults), len(cfCandidates))

		if len(cfCandidates) == 0 {
			logger.Warn("PUSH", "Cloudflare推送跳过: 无端口443的IP可用")
			fmt.Println("[WARN] Cloudflare推送跳过: 无端口443的IP可用")
		} else {
			pushCount := cfg.CFPushCount
			if pushCount > len(cfCandidates) {
				pushCount = len(cfCandidates)
			}
			logger.Info("PUSH", "准备推送到 Cloudflare DNS (前 %d 个端口443 IP)", pushCount)
			fmt.Printf("[INFO] 准备推送到 Cloudflare DNS (前 %d 个端口443 IP)\n", pushCount)

			cfPusher := pusher.NewCloudflarePusher(cfg)
			if err := cfPusher.Push(cfCandidates[:pushCount]); err != nil {
				logger.Error("PUSH", "Cloudflare 推送失败: %v", err)
				fmt.Fprintf(os.Stderr, "Cloudflare 推送失败: %v\n", err)
			} else {
				logger.Info("PUSH", "Cloudflare推送成功: %d 个IP (端口443)", pushCount)
				fmt.Printf("[INFO] Cloudflare推送成功: %d 个IP (端口443)\n", pushCount)
			}
		}
	} else if cfg.CFPushCount > 0 {
		logger.Warn("PUSH", "Cloudflare 配置不完整，跳过推送")
		fmt.Println("[WARN] Cloudflare 配置不完整，跳过推送")
	}

	// 8. 推送到 GitHub
	if cfg.GithubPushCount > 0 && cfg.GithubToken != "" && cfg.GithubRepo != "" {
		pushCount := cfg.GithubPushCount
		if pushCount > len(topResults) {
			pushCount = len(topResults)
		}
		logger.Info("PUSH", "准备推送到 GitHub (前 %d 个)", pushCount)
		fmt.Printf("[INFO] 准备推送到 GitHub (前 %d 个)\n", pushCount)

		// 生成 IP.txt (GitHub 推送也用)
		if err := output.ExportIPTxt(topResults, ipTxtPath, pushCount); err != nil {
			logger.Error("PUSH", "生成 IP.txt 失败: %v", err)
			fmt.Fprintf(os.Stderr, "生成 IP.txt 失败: %v\n", err)
		}

		githubPusher := pusher.NewGithubPusher(cfg)
		if err := githubPusher.Push(topResults[:pushCount]); err != nil {
			logger.Error("PUSH", "GitHub 推送失败: %v", err)
			fmt.Fprintf(os.Stderr, "GitHub 推送失败: %v\n", err)
		}
	} else if cfg.GithubPushCount > 0 {
		logger.Warn("PUSH", "GitHub 配置不完整，跳过推送")
		fmt.Println("[WARN] GitHub 配置不完整，跳过推送")
	}

	logger.Info("PUSH", "推送流程完成")
	fmt.Println("[INFO] 推送流程完成")
	return nil
}

// pushAfterBenchmark 采集入库后触发推送
// 包含数据验证、重试机制（最多3次，间隔5分钟）和失败告警通知
// collectStart 为采集开始时间，用于跳过刚采集的IP，仅重测历史IP
func pushAfterBenchmark(cfg *config.Config, resolver *geo.Resolver, db *repository.DB, collectStart time.Time) {
	// 1. 数据验证：确认入库数据完整
	dbCount, err := db.Count()
	if err != nil {
		logger.Error("PUSH", "采集后推送-数据验证失败: 查询数据库失败: %v", err)
		return
	}
	if dbCount == 0 {
		logger.Warn("PUSH", "采集后推送跳过: 数据库为空，无数据可推送")
		return
	}
	logger.Info("PUSH", "采集后推送-数据验证通过: 数据库 %d 条记录", dbCount)

	// 2. 推送重试（最多3次，间隔5分钟）
	maxRetries := 3
	retryInterval := 5 * time.Minute
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		pushStart := time.Now()
		logger.Info("PUSH", "采集后推送开始 (第%d/%d次尝试)", attempt, maxRetries)
		err := rerunAndPush(cfg, resolver, db, collectStart)
		pushDuration := time.Since(pushStart)

		if err == nil {
			// 推送成功，验证推送后数据一致性
			postCount, _ := db.Count()
			logger.Info("PUSH", "采集后推送成功 (第%d次, 耗时%.1fs) 数据量: 入库%d条/推送后%d条",
				attempt, pushDuration.Seconds(), dbCount, postCount)
			return
		}

		lastErr = err
		logger.Error("PUSH", "采集后推送失败 (第%d次, 耗时%.1fs): %v", attempt, pushDuration.Seconds(), err)

		if attempt < maxRetries {
			logger.Info("PUSH", "等待 %v 后重试...", retryInterval)
			time.Sleep(retryInterval)
		}
	}

	// 3. 全部失败，触发告警通知
	logger.Error("PUSH", "采集后推送 %d 次均失败，触发告警通知", maxRetries)
	if cfg.WxPusherEnable {
		wp := pusher.NewWxPusher(cfg)
		alarmMsg := fmt.Sprintf("采集后推送连续失败%d次\n最后错误: %v\n时间: %s\n数据库记录: %d",
			maxRetries, lastErr, time.Now().Format("2006-01-02 15:04:05"), dbCount)
		if err := wp.NotifyJobComplete("push", false, alarmMsg); err != nil {
			logger.Error("PUSH", "告警通知发送失败: %v", err)
		} else {
			logger.Info("PUSH", "告警通知已发送")
		}
	}
}

func outputResults(results []model.IPResult, cfg *config.Config, resolver *geo.Resolver, logger *log.Logger) {
	if len(results) == 0 {
		logger.Warn("SCORER", "没有有效的测速结果")
		fmt.Println("[WARN] 没有有效的测速结果")
		return
	}

	// 手动选择模式：按国家/地区过滤；混合模式不过滤（在推送阶段按50/50选择）
	if cfg.IPSelectMode == config.IPSelectModeManual && len(cfg.IPSelectCountries) > 0 {
		logger.Info("FILTER", "手动选择模式：筛选国家/地区 %v", cfg.IPSelectCountries)
		fmt.Printf("[INFO] 手动选择模式：筛选国家/地区: %v\n", cfg.IPSelectCountries)
		results = scorer.FilterByCountries(results, resolver, cfg.IPSelectCountries)
		logger.Info("FILTER", "过滤后剩余 %d 个结果", len(results))
		fmt.Printf("[INFO] 过滤后剩余 %d 个结果\n", len(results))

		if len(results) == 0 {
			logger.Warn("FILTER", "过滤后没有匹配的 IP 结果")
			fmt.Println("[WARN] 过滤后没有匹配的 IP 结果")
			return
		}
	} else if cfg.IPSelectMode == config.IPSelectModeHybrid {
		logger.Info("FILTER", "混合选择模式：自动+手动各50%%权重（推送时选择）")
		fmt.Printf("[INFO] 混合选择模式：自动+手动各50%%权重\n")
	} else {
		logger.Info("FILTER", "自动选择模式：按运营商线路自动选择")
		fmt.Printf("[INFO] 自动选择模式：按运营商线路自动选择\n")
	}

	if resolver.IsEnabled() {
		logger.Info("SCORER", "开始按运营商分组评分...")
		groupedResults := scorer.ScoreByISP(results, resolver, cfg)

		if len(groupedResults) == 0 {
			logger.Warn("SCORER", "没有有效的测速结果")
			fmt.Println("[WARN] 没有有效的测速结果")
			return
		}

		var allTopResults []model.IPResult
		for isp, topResults := range groupedResults {
			logger.Info("SCORER", "%s 线路评分完成，筛选出 %d 个最优结果", isp, len(topResults))
			fmt.Printf("\n=== %s 线路 Top %d ===\n", isp, cfg.TopN)
			output.PrintTable(topResults)

			allTopResults = append(allTopResults, topResults...)
		}

		// 全量 result.csv（所有线路合并）
		// 注: IP.txt 仅在推送环节生成（rerunAndPush），测速阶段不导出
		if err := output.ExportCSV(allTopResults, "result.csv"); err != nil {
			logger.Error("OUTPUT", "导出 result.csv 失败: %v", err)
			fmt.Fprintf(os.Stderr, "导出 result.csv 失败: %v\n", err)
		} else {
			logger.Info("OUTPUT", "全量结果已导出至 result.csv")
			fmt.Printf("全量结果已导出至 result.csv\n")
		}
	} else {
		logger.Info("SCORER", "开始综合评分排序...")
		topResults := scorer.Score(results, cfg)

		if topResults == nil || len(topResults) == 0 {
			logger.Warn("SCORER", "没有有效的测速结果")
			fmt.Println("[WARN] 没有有效的测速结果")
			return
		}
		logger.Info("SCORER", "评分完成，筛选出 %d 个最优结果", len(topResults))

		logger.Info("OUTPUT", "输出 Top %d 结果", cfg.TopN)
		fmt.Printf("\n=== 阶段 4: 输出 Top %d 结果 ===\n", cfg.TopN)
		output.PrintTable(topResults)

		if err := output.ExportCSV(topResults, "result.csv"); err != nil {
			logger.Error("OUTPUT", "导出 CSV 失败: %v", err)
			fmt.Fprintf(os.Stderr, "导出 CSV 失败: %v\n", err)
		} else {
			logger.Info("OUTPUT", "结果已导出至 result.csv")
			fmt.Printf("结果已导出至 result.csv\n")
		}
	}
}

func daemonize(logPath string) error {
	if isWindows() {
		return nil
	}

	// 设置 CF_DAEMONIZED=1 环境变量，让子进程知道已脱离终端，无需再次 daemonize
	// 这打破了 "子进程读 config.yaml → daemon_mode:true → 再次 daemonize" 的无限循环
	env := append(os.Environ(), "CF_DAEMONIZED=1")

	process, err := os.StartProcess(os.Args[0], os.Args, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   env,
	})
	if err != nil {
		return err
	}

	if logPath != "" {
		logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			os.Stdout = logFile
			os.Stderr = logFile
		}
	}

	process.Release()
	os.Exit(0)

	return nil
}

func isWindows() bool {
	return os.PathSeparator == '\\' || filepath.Separator == '\\'
}

// startUpdateChecker 启动后台版本检查 goroutine
// 流程:启动 30s 后首次检查(避免与初始化竞争),之后按 interval 间隔重复
// 检查失败不影响下一次 ticker;manifest 缓存在 Checker 内存中,Web 前端通过 /api/update/status 读取
// 关闭:依赖进程退出(未提供停止 channel,因 Checker.Check 自带 60s 超时,goroutine 不会泄漏)
func startUpdateChecker(checker *updater.Checker, interval time.Duration, lg *log.Logger) {
	if checker == nil {
		return
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	// 启动 30s 后首次检查(给 Web 服务、数据库等初始化留时间)
	time.Sleep(30 * time.Second)

	runCheck := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, _, err := checker.Check(ctx); err != nil {
			if lg != nil {
				lg.Warn("UPDATE", "后台版本检查失败: %v", err)
			}
		}
	}

	// 首次检查
	runCheck()

	// 后续按 interval 重复
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		runCheck()
	}
}
