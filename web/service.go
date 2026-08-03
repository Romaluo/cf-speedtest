package web

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"cf-speedtest/cleanup"
	"cf-speedtest/collector"
	"cf-speedtest/config"
	"cf-speedtest/engine"
	"cf-speedtest/geo"
	"cf-speedtest/model"
	"cf-speedtest/output"
	"cf-speedtest/pusher"
	"cf-speedtest/scorer"
)

// jobStatus 任务状态
type jobStatus string

const (
	statusRunning  jobStatus = "running"
	statusDone     jobStatus = "completed"
	statusFailed   jobStatus = "failed"
	statusCanceled jobStatus = "canceled"
)

const maxJobLogs = 200

// Job 单次触发任务
type Job struct {
	ID         string
	Type       string // benchmark / push
	Status     jobStatus
	Progress   int
	Total      int
	Message    string
	StartedAt  time.Time
	FinishedAt *time.Time
	logs       []string
	mu         sync.Mutex
	cancelCh   chan struct{} // 取消信号通道,close 后通知任务退出
	cancelOnce sync.Once     // 确保 cancelCh 只关闭一次
}

// jobView 任务的 JSON 视图（不含互斥锁，可安全序列化）
type jobView struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Status     jobStatus  `json:"status"`
	Progress   int        `json:"progress"`
	Total      int        `json:"total"`
	Message    string     `json:"message"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	LogLines   []string   `json:"logs"`
}

func (j *Job) addLog(format string, args ...interface{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	j.logs = append(j.logs, line)
	if len(j.logs) > maxJobLogs {
		j.logs = j.logs[len(j.logs)-maxJobLogs:]
	}
}

func (j *Job) setProgress(cur, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Progress = cur
	j.Total = total
}

func (j *Job) setMessage(format string, args ...interface{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Message = fmt.Sprintf(format, args...)
}

// setStatus 线程安全地更新任务状态
func (j *Job) setStatus(s jobStatus) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = s
}

// status 线程安全地读取任务状态
func (j *Job) status() jobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status
}

// Cancel 发送取消信号并标记任务为 canceled
// 通过 close(cancelCh) 通知正在运行的 goroutine 退出
func (j *Job) Cancel() {
	j.cancelOnce.Do(func() {
		close(j.cancelCh)
	})
	j.setStatus(statusCanceled)
}

// IsCanceled 检查任务是否已被取消(非阻塞)
func (j *Job) IsCanceled() bool {
	select {
	case <-j.cancelCh:
		return true
	default:
		return false
	}
}

// fail 标记任务失败
func (j *Job) fail(msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = statusFailed
	j.Message = msg
	now := time.Now()
	j.FinishedAt = &now
	j.logs = append(j.logs, fmt.Sprintf("[%s] ❌ %s", now.Format("15:04:05"), msg))
	if len(j.logs) > maxJobLogs {
		j.logs = j.logs[len(j.logs)-maxJobLogs:]
	}
}

// view 返回用于 JSON 序列化的只读视图（拷贝日志切片，避免复制互斥锁）
func (j *Job) view() *jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	logs := make([]string, len(j.logs))
	copy(logs, j.logs)
	return &jobView{
		ID:         j.ID,
		Type:       j.Type,
		Status:     j.Status,
		Progress:   j.Progress,
		Total:      j.Total,
		Message:    j.Message,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		LogLines:   logs,
	}
}

// jobTracker 任务跟踪器
type jobTracker struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	order []string // 按 ID 顺序保留，便于清理
}

func newJobTracker() *jobTracker {
	return &jobTracker{jobs: make(map[string]*Job)}
}

func (t *jobTracker) create(id, typ string) *Job {
	j := &Job{
		ID:        id,
		Type:      typ,
		Status:    statusRunning,
		StartedAt: time.Now(),
		cancelCh:  make(chan struct{}),
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 清除所有已完成的旧任务,只保留当前正在运行的和最新的一个
	keep := make([]string, 0, 2)
	for _, oid := range t.order {
		if old, ok := t.jobs[oid]; ok {
			// 保留正在运行的任务
			if old.status() == statusRunning {
				keep = append(keep, oid)
				continue
			}
			// 已完成的旧任务直接删除
			delete(t.jobs, oid)
		}
	}
	t.order = keep
	t.jobs[id] = j
	t.order = append(t.order, id)
	return j
}

func (t *jobTracker) get(id string) (*Job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	return j, ok
}

func (t *jobTracker) list() []*jobView {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]*jobView, 0, len(t.order))
	// 倒序（最新在前）
	for i := len(t.order) - 1; i >= 0; i-- {
		if j, ok := t.jobs[t.order[i]]; ok {
			result = append(result, j.view())
		}
	}
	return result
}

func (t *jobTracker) current() *Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.order) - 1; i >= 0; i-- {
		if j, ok := t.jobs[t.order[i]]; ok && j.status() == statusRunning {
			return j
		}
	}
	return nil
}

// currentView 返回当前运行任务的只读视图
func (t *jobTracker) currentView() *jobView {
	if j := t.current(); j != nil {
		return j.view()
	}
	return nil
}

// runBenchmark 触发一次完整测速（拉取 IP + 增量测速 + 评分 + 入库）
// 注意：手动"立即测速"按钮仅完成采集入库，不执行任何推送。推送由"立即推送"按钮独立触发。
func (srv *Server) runBenchmark(job *Job) {
	defer srv.finishJob(job)

	// 资源清理：任务结束后执行（defer LIFO，先于 finishJob 执行）
	var baseline *cleanup.ResourceSnapshot
	if srv.cleaner != nil {
		baseline = cleanup.Snapshot()
		defer func() {
			result := srv.cleaner.Cleanup(config.JobTypeBenchmark, baseline, srv.deps.DB)
			job.addLog("资源清理: 内存释放 %.1f%%, 临时文件 %d, 进程 %d, 耗时 %s",
				result.MemoryRatio*100, result.TempFilesDeleted, result.ProcessesKilled, result.Duration.Round(time.Millisecond))
		}()
	}

	cfg := srv.deps.Cfg
	db := srv.deps.DB

	job.addLog("开始获取 IP（官方地址: %v, 数量: %d）...", cfg.IPv4Enabled, cfg.IPv4Count)
	job.setMessage("拉取 IP 中")
	fetcher := collector.NewFetcherWithStats(cfg, srv.deps.CIDRStats)
	fetchRes, err := fetcher.FetchCategorized(func(p collector.FetchProgress) {
		job.addLog("拉取进度[%s] %s: 本URL %d 个, 累计 官方%d+自定义%d=%d",
			p.Source, p.URL, p.Count, p.Official, p.Custom, p.Total)
		job.setMessage("拉取 IP 中: 官方 %d + 自定义 %d = %d", p.Official, p.Custom, p.Total)
	})
	if err != nil {
		job.setMessage("获取 IP 失败: %v", err)
		job.setStatus(statusFailed)
		srv.deps.Logger.Error("WEB", "触发测速-获取IP失败: %v", err)
		return
	}
	job.addLog("拉取完成: 官方 %d + 自定义 %d = %d 个 IP",
		len(fetchRes.OfficialTasks), len(fetchRes.CustomTasks),
		len(fetchRes.OfficialTasks)+len(fetchRes.CustomTasks))

	// 取消检查: IP 拉取后
	if job.IsCanceled() {
		job.addLog("任务已取消（IP 拉取后）")
		return
	}

	// 数据整合: 合并官方与自定义地址的 IP
	allTasks := make([]model.Task, 0, len(fetchRes.OfficialTasks)+len(fetchRes.CustomTasks))
	allTasks = append(allTasks, fetchRes.OfficialTasks...)
	allTasks = append(allTasks, fetchRes.CustomTasks...)
	if len(allTasks) == 0 {
		job.setMessage("未获取到任何 IP")
		job.setStatus(statusFailed)
		return
	}
	job.addLog("共获取 %d 个 IP（合并后）", len(allTasks))

	// 端口预过滤: 清除数据库中不在用户配置端口列表中的旧记录
	if len(cfg.TCPPingPorts) > 0 {
		deleted, err := db.DeleteByPortsNotIn(cfg.TCPPingPorts)
		if err != nil {
			job.addLog("端口预过滤失败: %v", err)
			srv.deps.Logger.Error("WEB", "端口预过滤失败: %v", err)
			job.setStatus(statusFailed)
			job.setMessage("端口预过滤失败，已中止测速")
			return
		}
		if deleted > 0 {
			job.addLog("端口预过滤: 清除非配置端口旧记录 %d 条", deleted)
			srv.deps.Logger.Info("WEB", "端口预过滤: 清除非配置端口记录 %d 条", deleted)
		}
		// 验证: 确认数据库中不再有非配置端口记录
		remaining, err := db.CountByPortsNotIn(cfg.TCPPingPorts)
		if err != nil {
			job.addLog("端口验证查询失败: %v", err)
		} else if remaining > 0 {
			job.addLog("端口验证警告: 仍有 %d 条非配置端口记录，再次清理", remaining)
			db.DeleteByPortsNotIn(cfg.TCPPingPorts)
		} else {
			job.addLog("端口验证通过: 数据库仅含配置端口 %v 的记录", cfg.TCPPingPorts)
		}
	}

	// 数据清洗: 清除已有的下载速度为0的记录
	if deleted, err := db.DeleteZeroSpeed(); err == nil && deleted > 0 {
		job.addLog("数据清洗: 清除 %d 条下载速度为0的旧记录", deleted)
		srv.deps.Logger.Info("WEB", "数据清洗: 清除 %d 条0Mbps记录", deleted)
	}

	// 增量: 跳过未过期 IP
	validIPs, err := db.GetValidIPs(cfg.IPExpireTime)
	if err != nil {
		job.addLog("查询有效IP失败: %v（按全量测速）", err)
	}
	var tasks []model.Task
	for _, t := range allTasks {
		key := fmt.Sprintf("%s:%d", t.IP, t.Port)
		if !validIPs[key] {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		job.setMessage("所有 IP 均在有效期内，无需重测（共 %d 个）", len(allTasks))
		job.addLog("所有 IP 均在有效期内，无需重测")
		job.Total = len(allTasks)
		job.Progress = len(allTasks)
		return
	}
	job.addLog("需测速 %d 个，跳过缓存 %d 个", len(tasks), len(allTasks)-len(tasks))

	// TCP 握手预筛选: 并发测试连通性，清除不可达 IP
	job.setMessage("TCP握手预筛选中: 0/%d", len(tasks))
	preTimeout := 3 * time.Second
	if cfg.TCPPingTimeout > 0 {
		preTimeout = cfg.TCPPingTimeout
	}
	preResults, preStats := engine.PreFilterTCP(tasks, cfg.Concurrency, preTimeout)
	job.addLog("TCP握手预筛选完成: 总计 %d, 成功 %d, 失败 %d, 耗时 %s, 平均延迟 %s",
		preStats.Total, preStats.Success, preStats.Failed,
		preStats.Duration.Round(time.Millisecond), preStats.AvgLatency.Round(time.Millisecond))
	srv.deps.Logger.Info("WEB", "TCP握手预筛选: %d → %d (失败 %d, 耗时 %s)",
		preStats.Total, preStats.Success, preStats.Failed, preStats.Duration.Round(time.Millisecond))

	// 仅保留握手成功的 IP 进入测速
	tasks = engine.FilterReachable(tasks, preResults)
	if len(tasks) == 0 {
		job.setMessage("TCP握手预筛选后无可用IP")
		job.addLog("所有IP握手失败，无可用IP进入测速")
		job.setStatus(statusFailed)
		return
	}
	job.addLog("预筛选后剩余 %d 个IP进入测速", len(tasks))

	// 官方地址补全国家代码：xdb快速初筛（不做trace验证，仅用于初步过滤）
	engine.FillCountryCodes(tasks, srv.deps.Resolver, false)

	// 取消检查: 测速前
	if job.IsCanceled() {
		job.addLog("任务已取消（测速前）")
		return
	}

	// 手动模式: 按用户指定国家筛选（测速前预过滤，避免对不符合条件的IP浪费测速资源）
	if cfg.IPSelectMode == config.IPSelectModeManual && len(cfg.IPSelectCountries) > 0 {
		beforeFilter := len(tasks)
		tasks = engine.FilterTasksByCountries(tasks, cfg.IPSelectCountries)
		job.addLog("国家筛选[%v]: %d → %d", cfg.IPSelectCountries, beforeFilter, len(tasks))
		srv.deps.Logger.Info("WEB", "手动模式国家筛选: %d → %d (%v)", beforeFilter, len(tasks), cfg.IPSelectCountries)
		if len(tasks) == 0 {
			job.setMessage("国家筛选后无可用IP（指定国家: %v）", cfg.IPSelectCountries)
			job.setStatus(statusFailed)
			return
		}

		// 纠错验证：对筛选后的IP做cdn-cgi/trace精准验证，纠正xdb误判
		// 纠错后再次过滤：不在用户选择国家中的IP直接删除，不占用测速资源
		if cfg.TraceVerifyEnable && srv.deps.Resolver != nil && srv.deps.Resolver.IsEnabled() {
			verifyConcurrency := cfg.TraceVerifyConcurrency
			if verifyConcurrency <= 0 {
				verifyConcurrency = 10
			}
			beforeVerify := len(tasks)
			var corrected, failed int
			tasks, corrected, failed = geo.VerifyTasksCountryCodes(tasks, srv.deps.Resolver, verifyConcurrency)
			job.addLog("纠错验证: %d 个IP, 修正 %d 条, 失败(移除) %d 条, 剩余 %d 条",
				beforeVerify, corrected, failed, len(tasks))
			srv.deps.Logger.Info("WEB", "纠错验证: %d 个IP, 修正 %d 条, 失败(移除) %d 条, 剩余 %d 条",
				beforeVerify, corrected, failed, len(tasks))
			// 纠错后重新过滤（纠正后国家代码可能不再符合用户选择）
			afterVerify := len(tasks)
			tasks = engine.FilterTasksByCountries(tasks, cfg.IPSelectCountries)
			removed := afterVerify - len(tasks)
			if removed > 0 {
				job.addLog("纠错后重新筛选: 移除 %d 条不属于目标国家的IP", removed)
			}
			if len(tasks) == 0 {
				job.setMessage("纠错验证后无可用IP")
				job.setStatus(statusFailed)
				return
			}
			// 更新 CIDR 权重统计：记录各 CIDR 通过验证+国家筛选的 IP 数
			if srv.deps.CIDRStats != nil {
				passedPerCIDR := make(map[string]int)
				for _, t := range tasks {
					if t.SourceCIDR != "" {
						passedPerCIDR[t.SourceCIDR]++
					}
				}
				for cidr, cnt := range passedPerCIDR {
					srv.deps.CIDRStats.RecordPassed(cidr, cnt)
				}
				if err := srv.deps.CIDRStats.Save(); err != nil {
					srv.deps.Logger.Warn("WEB", "CIDR 统计保存失败: %v", err)
				} else {
					job.addLog("CIDR 权重已更新: %d 个 CIDR", len(passedPerCIDR))
					srv.deps.Logger.Info("WEB", "CIDR 权重统计已更新并保存")
				}
			}
		}
	}
	job.setMessage("测速中: 0/%d", len(tasks))

	bench := engine.NewBenchmarkEngine(cfg, srv.deps.Resolver)
	resultCh := bench.Run(tasks)

	// 分块流式处理: 每收集 BATCH_SIZE 个结果就评分+入库+释放内存
	const BATCH_SIZE = 50
	var scoredBuffer []model.IPResult // 本批次已评分结果
	var totalResults int
	completed := 0
	total := len(tasks)

	// P1-6: 收集失败的任务（用于批次降级重试）
	var failedTasks []model.Task
	failedSet := make(map[string]bool) // "ip:port" 去重

	flush := func() {
		if len(scoredBuffer) == 0 {
			return
		}
		// 按分数排序
		sort.Slice(scoredBuffer, func(i, j int) bool {
			return scoredBuffer[i].Score > scoredBuffer[j].Score
		})
		// 批量入库
		if err := db.BatchUpsert(scoredBuffer); err != nil {
			job.addLog("写入数据库失败: %v", err)
		} else {
			totalResults += len(scoredBuffer)
		}
		// 释放内存
		scoredBuffer = nil
	}

	errCount := 0
	// 调试统计
	var dbgSuccess, dbgAfterSpeed, dbgAfterScore int
	countryDist := make(map[string]int) // 国家分布统计
	for r := range resultCh {
		// 取消检查: 收到取消信号后排空通道并退出
		if job.IsCanceled() {
			job.addLog("收到取消信号，正在终止测速并清理资源...")
			job.setMessage("任务已取消，正在清理...")
			for range resultCh { // 排空剩余结果，避免 goroutine 泄漏
			}
			flush()
			job.addLog("任务终止完成")
			return
		}
		if r.Err != nil {
			errCount++
			// 记录前10个错误详情
			if errCount <= 10 {
				job.addLog("  [%s:%d] 错误: %v", r.IP, r.Port, r.Err)
			}
			// P1-6: 收集失败任务用于降级重试
			key := fmt.Sprintf("%s:%d", r.IP, r.Port)
			if !failedSet[key] {
				failedSet[key] = true
				failedTasks = append(failedTasks, model.Task{
					IP:          r.IP,
					Port:        r.Port,
					CountryCode: r.CountryCode,
				})
			}
		}
		// 记录国家分布（测速成功的IP）
		if r.Err == nil {
			dbgSuccess++
			cc := r.CountryCode
			if cc == "" || cc == "-" {
				cc = "未知"
			}
			countryDist[cc]++
		}
		// 单条结果即评分+过滤（国家筛选已在测速前完成）
		scored := scorer.ScoreAllResults([]model.IPResult{r}, cfg)
		for _, s := range scored {
			// 数据清洗: 移除下载速度为0的IP（下载失败或无带宽）
			if s.DownloadSpeed <= 0 {
				continue
			}
			// 硬性规则验证
			if !cfg.PassesHardRules(s) {
				continue
			}
			dbgAfterSpeed++
			if s.Score >= cfg.MinScoreThreshold {
				scoredBuffer = append(scoredBuffer, s)
				dbgAfterScore++
			}
		}
		// 达到批大小则刷写
		if len(scoredBuffer) >= BATCH_SIZE {
			flush()
		}
		completed++
		if completed%100 == 0 || completed == total {
			job.setProgress(completed, total)
			job.setMessage("测速中: %d/%d (已入库 %d, 失败 %d)", completed, total, totalResults, errCount)
		}
	}
	// 刷写剩余结果
	flush()

	// P1-6: 批次降级重试 — 当失败 IP 较多时，降低并发重试一次
	if !job.IsCanceled() && cfg.RetryBatchFallback && len(failedTasks) > 0 {
		// 仅当失败率 > 30% 时触发降级（避免无谓重试）
		failRate := float64(len(failedTasks)) / float64(total)
		if failRate >= 0.3 && len(failedTasks) <= total {
			retryCfg := *cfg
			if retryCfg.Concurrency > 1 {
				retryCfg.Concurrency = retryCfg.Concurrency / 2
				if retryCfg.Concurrency < 1 {
					retryCfg.Concurrency = 1
				}
			}
			// 降级重试时不再次触发降级，避免无限循环
			retryCfg.RetryBatchFallback = false
			job.addLog("批次降级重试: %d 个失败IP，并发降至 %d", len(failedTasks), retryCfg.Concurrency)
			srv.deps.Logger.Info("WEB", "P1-6 批次降级重试: %d IP, 并发 %d", len(failedTasks), retryCfg.Concurrency)

			retryBench := engine.NewBenchmarkEngine(&retryCfg, srv.deps.Resolver)
			retryCh := retryBench.Run(failedTasks)
			retryFailed := 0
			retrySuccess := 0
			for r := range retryCh {
				if r.Err != nil {
					retryFailed++
					continue
				}
				retrySuccess++
				// 评分并入库
				scored := scorer.ScoreAllResults([]model.IPResult{r}, cfg)
				for _, s := range scored {
					if s.DownloadSpeed <= 0 {
						continue
					}
					if !cfg.PassesHardRules(s) {
						continue
					}
					if s.Score >= cfg.MinScoreThreshold {
						scoredBuffer = append(scoredBuffer, s)
						if len(scoredBuffer) >= BATCH_SIZE {
							flush()
						}
					}
				}
			}
			flush()
			if retrySuccess > 0 {
				job.addLog("批次降级重试完成: 成功 %d, 失败 %d", retrySuccess, retryFailed)
				errCount -= retrySuccess
				dbgSuccess += retrySuccess
			}
		}
	}

	job.addLog("测速完成: 成功 %d, 失败 %d, 有效入库 %d 个", completed-errCount, errCount, totalResults)
	// 调试统计: 显示各阶段过滤情况（国家筛选已在测速前完成）
	job.addLog("过滤统计: 测速成功 %d → 0Mbps过滤后 %d → 阈值过滤后 %d",
		dbgSuccess, dbgAfterSpeed, dbgAfterScore)
	srv.deps.Logger.Info("WEB", "过滤统计: 成功 %d → 带宽 %d → 阈值 %d",
		dbgSuccess, dbgAfterSpeed, dbgAfterScore)
	// 调试: 国家分布（显示测速成功IP的国家分布）
	if len(countryDist) > 0 {
		job.addLog("国家分布(测速成功 %d 个): %v", dbgSuccess, countryDist)
		srv.deps.Logger.Info("WEB", "国家分布: %v", countryDist)
	}

	if totalResults == 0 {
		job.setMessage("无有效结果可入库")
		return
	}

	// 末位淘汰
	if cfg.MaxDBSize > 0 {
		if cnt, _ := db.Count(); cnt > int64(cfg.MaxDBSize) {
			deleted, _ := db.TrimToMaxSize(cfg.MaxDBSize)
			job.addLog("末位淘汰: 删除 %d 条低分记录", deleted)
		}
	}

	// 清理过期
	if deleted, err := db.CleanupOldData(cfg.DataRetention); err == nil && deleted > 0 {
		job.addLog("清理过期记录 %d 条", deleted)
	}

	cnt, _ := db.Count()
	job.setMessage("测速完成: 入库 %d 条，数据库共 %d 条", totalResults, cnt)
	job.setProgress(total, total)
	srv.deps.Logger.Info("WEB", "触发测速完成: 入库 %d 条", totalResults)

	// 手动"立即测速"仅完成采集入库，不触发推送。
	// 推送由"立即推送"按钮（runPush）或定时任务（runDaemon）独立触发。
}

// runPush 推送流程（手动触发，重测全部IP）
func (srv *Server) runPush(job *Job) {
	defer srv.finishJob(job)
	srv.runPushWithSkipAfter(job, time.Time{})
}

// runPushWithSkipAfter 推送核心逻辑
// skipAfter 非零时仅重测历史IP（跳过刚采集的），用于采集后推送
func (srv *Server) runPushWithSkipAfter(job *Job, skipAfter time.Time) {
	// 资源清理：任务结束后执行（defer LIFO，先于 finishJob 执行）
	var baseline *cleanup.ResourceSnapshot
	if srv.cleaner != nil {
		baseline = cleanup.Snapshot()
		defer func() {
			result := srv.cleaner.Cleanup(config.JobTypePush, baseline, srv.deps.DB)
			job.addLog("资源清理: 内存释放 %.1f%%, 临时文件 %d, 进程 %d, 耗时 %s",
				result.MemoryRatio*100, result.TempFilesDeleted, result.ProcessesKilled, result.Duration.Round(time.Millisecond))
		}()
	}

	cfg := srv.deps.Cfg
	db := srv.deps.DB

	// 1. 获取数据库中 IP（skipAfter 非零时仅获取历史IP，跳过刚采集的）
	job.addLog("步骤1: 从数据库读取 IP (端口: %v)", cfg.TCPPingPorts)
	var allTasks []model.Task
	var err error
	if !skipAfter.IsZero() {
		job.addLog("采集后推送模式: 仅重测 %s 之前更新的历史IP", skipAfter.Format("15:04:05"))
		if len(cfg.TCPPingPorts) > 0 {
			allTasks, err = db.GetIPsByPortsBefore(cfg.TCPPingPorts, skipAfter)
		} else {
			allTasks, err = db.GetAllIPsBefore(skipAfter)
		}
	} else {
		if len(cfg.TCPPingPorts) > 0 {
			allTasks, err = db.GetIPsByPorts(cfg.TCPPingPorts)
		} else {
			allTasks, err = db.GetAllIPs()
		}
	}
	if err != nil {
		job.setMessage("读取数据库失败: %v", err)
		job.setStatus(statusFailed)
		return
	}
	if len(allTasks) == 0 {
		job.setMessage("数据库无结果可推送（端口: %v）", cfg.TCPPingPorts)
		job.addLog("数据库中没有匹配端口 %v 的 IP", cfg.TCPPingPorts)
		job.setStatus(statusFailed)
		return
	}
	job.addLog("共读取 %d 个 IP (端口: %v)", len(allTasks), cfg.TCPPingPorts)

	// 取消检查: 读取数据库后
	if job.IsCanceled() {
		job.addLog("任务已取消（读取数据库后）")
		return
	}

	// 2. TCP握手预筛选（与测速流程一致，先过滤不可达IP）
	job.addLog("步骤2: TCP握手预筛选 %d 个 IP (并发: %d, 超时: %s)", len(allTasks), cfg.Concurrency, cfg.TCPPingTimeout)
	job.setMessage("TCP握手预筛选中: 0/%d", len(allTasks))
	preResults, stats := engine.PreFilterTCP(allTasks, cfg.Concurrency, cfg.TCPPingTimeout)
	reachableTasks := engine.FilterReachable(allTasks, preResults)
	job.addLog("TCP握手预筛选完成: 总计 %d, 成功 %d, 失败 %d, 耗时 %s, 平均延迟 %s",
		stats.Total, stats.Success, stats.Failed, stats.Duration.Round(time.Millisecond), stats.AvgLatency.Round(time.Millisecond))
	srv.deps.Logger.Info("WEB", "推送前TCP预筛选: %d → %d (失败 %d)", stats.Total, stats.Success, stats.Failed)
	if len(reachableTasks) == 0 {
		job.setMessage("TCP握手预筛选后无可用IP")
		job.setStatus(statusFailed)
		return
	}

	// 3. 全量重测（仅握手成功的IP）
	job.addLog("步骤3: 全量重测 %d 个 IP (并发: %d)", len(reachableTasks), cfg.Concurrency)
	job.setMessage("重测中: 0/%d", len(reachableTasks))

	bench := engine.NewBenchmarkEngine(cfg, srv.deps.Resolver)
	resultCh := bench.Run(reachableTasks)

	var reResults []model.IPResult
	completed := 0
	total := len(reachableTasks)
	errCount := 0
	// P1-6: 收集失败任务用于批次降级重试
	var failedTasks []model.Task
	failedSet := make(map[string]bool)
	for r := range resultCh {
		// 取消检查: 收到取消信号后排空通道并退出
		if job.IsCanceled() {
			job.addLog("收到取消信号，正在终止重测并清理资源...")
			job.setMessage("任务已取消，正在清理...")
			for range resultCh { // 排空剩余结果，避免 goroutine 泄漏
			}
			job.addLog("任务终止完成")
			return
		}
		reResults = append(reResults, r)
		if r.Err != nil {
			errCount++
			if errCount <= 10 {
				job.addLog("  [%s:%d] 错误: %v", r.IP, r.Port, r.Err)
			}
			// 收集失败任务（按 ip:port 去重）
			key := fmt.Sprintf("%s:%d", r.IP, r.Port)
			if !failedSet[key] {
				failedSet[key] = true
				failedTasks = append(failedTasks, model.Task{
					IP:          r.IP,
					Port:        r.Port,
					CountryCode: r.CountryCode,
				})
			}
		}
		completed++
		if completed%100 == 0 || completed == total {
			job.setProgress(completed, total)
			job.setMessage("重测中: %d/%d (失败 %d)", completed, total, errCount)
		}
	}
	job.addLog("步骤3完成: 重测 %d 个 IP, 成功 %d, 失败 %d", len(reResults), len(reResults)-errCount, errCount)

	// P1-6: 批次降级重试 — 当失败 IP 较多时，降低并发重试一次（与 runBenchmark 保持一致）
	if !job.IsCanceled() && cfg.RetryBatchFallback && len(failedTasks) > 0 {
		failRate := float64(len(failedTasks)) / float64(total)
		if failRate >= 0.3 && len(failedTasks) <= total {
			retryCfg := *cfg
			if retryCfg.Concurrency > 1 {
				retryCfg.Concurrency = retryCfg.Concurrency / 2
				if retryCfg.Concurrency < 1 {
					retryCfg.Concurrency = 1
				}
			}
			// 降级重试时不再次触发降级，避免无限循环
			retryCfg.RetryBatchFallback = false
			job.addLog("步骤3批次降级重试: %d 个失败IP，并发降至 %d (失败率 %.0f%%)",
				len(failedTasks), retryCfg.Concurrency, failRate*100)
			srv.deps.Logger.Info("WEB", "P1-6 推送前批次降级重试: %d IP, 并发 %d", len(failedTasks), retryCfg.Concurrency)

			retryBench := engine.NewBenchmarkEngine(&retryCfg, srv.deps.Resolver)
			retryCh := retryBench.Run(failedTasks)
			retryFailed := 0
			retrySuccess := 0
			for r := range retryCh {
				if r.Err != nil {
					retryFailed++
					continue
				}
				retrySuccess++
				// 重试成功的 IP 追加到结果列表（失败版本会在步骤4数据清洗时按 DownloadSpeed<=0 过滤）
				reResults = append(reResults, r)
			}
			if retrySuccess > 0 {
				job.addLog("步骤3批次降级重试完成: 成功 %d, 失败 %d", retrySuccess, retryFailed)
				errCount -= retrySuccess
				srv.deps.Logger.Info("WEB", "P1-6 推送前批次降级重试: 成功 %d, 失败 %d", retrySuccess, retryFailed)
			}
		}
	}

	// 4. 100分制评分 + 排序 + 数据清洗
	// 取消检查: 评分前
	if job.IsCanceled() {
		job.addLog("任务已取消（评分前）")
		return
	}
	job.addLog("步骤4: 100分制评分排序")
	scoredResults := scorer.ScoreAllResults(reResults, cfg)
	if scoredResults == nil || len(scoredResults) == 0 {
		job.setMessage("重测无有效结果 (成功 %d, 失败 %d)", len(reResults)-errCount, errCount)
		job.addLog("重测无有效结果: 所有 IP 测试均失败")
		job.setStatus(statusFailed)
		return
	}
	// 数据清洗: 移除下载速度为0的IP + 硬性规则验证
	beforeClean := len(scoredResults)
	var cleanedResults []model.IPResult
	for _, r := range scoredResults {
		if r.DownloadSpeed <= 0 {
			continue
		}
		if !cfg.PassesHardRules(r) {
			continue
		}
		cleanedResults = append(cleanedResults, r)
	}
	scoredResults = cleanedResults
	if zeroRemoved := beforeClean - len(scoredResults); zeroRemoved > 0 {
		job.addLog("数据清洗: 移除 %d 条下载速度为0的IP", zeroRemoved)
		srv.deps.Logger.Info("WEB", "推送前数据清洗: 移除 %d 条0Mbps记录", zeroRemoved)
	}
	if len(scoredResults) == 0 {
		job.setMessage("数据清洗后无有效IP（所有IP下载速度为0）")
		job.setStatus(statusFailed)
		return
	}
	sort.Slice(scoredResults, func(i, j int) bool {
		return scoredResults[i].Score > scoredResults[j].Score
	})
	job.addLog("评分完成，共 %d 个有效结果（清洗后）", len(scoredResults))

	// 5. 更新数据库
	job.addLog("步骤5: 更新数据库")
	if err := db.BatchUpsert(scoredResults); err != nil {
		job.addLog("更新数据库失败: %v", err)
		srv.deps.Logger.Error("WEB", "更新数据库失败: %v", err)
	}

	if cfg.MaxDBSize > 0 {
		currentCount, _ := db.Count()
		if currentCount > int64(cfg.MaxDBSize) {
			deleted, _ := db.TrimToMaxSize(cfg.MaxDBSize)
			job.addLog("末位淘汰: 删除 %d 条低分记录", deleted)
		}
	}

	// 5. 取前N个结果写入 IP.txt
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

	var topResults []model.IPResult
	if cfg.IPSelectMode == config.IPSelectModeHybrid && len(cfg.IPSelectCountries) > 0 {
		// 混合模式：50%来自手动指定国家，50%来自其他
		topResults = scorer.SelectHybridTopN(scoredResults, cfg.IPSelectCountries, topN)
		job.addLog("步骤6: 混合模式选择 %d 个结果（50%%手动 + 50%%自动）", len(topResults))
	} else {
		topResults = scoredResults[:topN]
		job.addLog("步骤6: 取前 %d 个结果写入 IP.txt", topN)
	}

	ipTxtPath := cfg.GithubFilePath
	if ipTxtPath == "" {
		ipTxtPath = "IP.txt"
	}
	if err := output.ExportIPTxt(topResults, ipTxtPath, topN); err != nil {
		job.addLog("生成 IP.txt 失败: %v", err)
	} else {
		job.addLog("IP.txt 已生成: %s", ipTxtPath)
	}

	// 6. 补全归属地信息
	if srv.deps.Resolver.IsEnabled() {
		localISP := srv.deps.Resolver.GetLocalISP()
		for i := range topResults {
			if topResults[i].ISP == "" {
				topResults[i].ISP = localISP
			}
			if topResults[i].CountryCode == "" || topResults[i].CountryCode == "-" {
				info := srv.deps.Resolver.Lookup(topResults[i].IP)
				if info != nil && info.Country != "" && info.Country != "0" {
					topResults[i].CountryCode = scorer.GetCountryCode(info.Country)
				}
			}
		}
	}

	// 7. Cloudflare 推送（仅推送端口443的IP，固定取前10个）
	// 取消检查: 推送前
	if job.IsCanceled() {
		job.addLog("任务已取消（推送前）")
		return
	}
	cfOK := false
	if cfg.CFPushCount > 0 && cfg.CFAPIKey != "" && cfg.CFZoneID != "" && cfg.CFDNSName != "" {
		// 筛选端口为 443 的 IP（Cloudflare DNS 仅支持标准 HTTPS 端口）
		var cfCandidates []model.IPResult
		for _, r := range topResults {
			if r.Port == 443 {
				cfCandidates = append(cfCandidates, r)
			}
		}
		job.addLog("Cloudflare DNS 推送: 筛选端口443 → %d/%d 个候选", len(cfCandidates), len(topResults))
		srv.deps.Logger.Info("WEB", "Cloudflare推送端口筛选: %d → %d (仅443)", len(topResults), len(cfCandidates))

		// P2-9: IP 风险等级过滤（DNS 推送前过滤高风险 IP）
		if cfg.IPRiskFilterEnable && len(cfCandidates) > 0 {
			beforeRisk := len(cfCandidates)
			job.addLog("IP风险过滤: 查询 %d 个 IP 的风险分数 (阈值 >%d)...", beforeRisk, cfg.IPRiskScoreThreshold)
			job.setMessage("IP风险过滤中: 0/%d", beforeRisk)
			safeCandidates, riskChecks := engine.FilterByRisk(cfCandidates, cfg.IPRiskScoreThreshold, cfg.IPRiskFilterTimeout, cfg.Concurrency)
			filteredCount := beforeRisk - len(safeCandidates)
			job.addLog("IP风险过滤完成: %d → %d (过滤 %d 个高风险)",
				beforeRisk, len(safeCandidates), filteredCount)
			srv.deps.Logger.Info("WEB", "P2-9 IP风险过滤: %d → %d (过滤 %d, 阈值 >%d)",
				beforeRisk, len(safeCandidates), filteredCount, cfg.IPRiskScoreThreshold)
			// 记录被过滤的 IP 详情
			for _, rc := range riskChecks {
				if !rc.Safe && rc.RiskScore >= 0 {
					job.addLog("  IP %s: risk_score=%d (%s) → 过滤", rc.IP, rc.RiskScore, rc.Reason)
				}
			}
			cfCandidates = safeCandidates
		}

		if len(cfCandidates) == 0 {
			job.addLog("Cloudflare DNS 推送: 无端口443的IP可用，跳过")
			srv.deps.Logger.Warn("WEB", "Cloudflare推送跳过: 无端口443的IP")
		} else {
			cfPushCount := cfg.CFPushCount
			if cfPushCount > len(cfCandidates) {
				cfPushCount = len(cfCandidates)
			}
			job.addLog("Cloudflare DNS 推送: 取前 %d 个端口443 IP", cfPushCount)
			job.setMessage("推送 Cloudflare DNS 中...")
			cfPusher := pusher.NewCloudflarePusher(cfg)
			if err := cfPusher.Push(cfCandidates[:cfPushCount]); err != nil {
				job.addLog("Cloudflare 推送失败: %v", err)
				srv.deps.Logger.Error("WEB", "Cloudflare 推送失败: %v", err)
			} else {
				cfOK = true
				job.addLog("Cloudflare DNS 推送成功 (%d 个端口443 IP)", cfPushCount)
				srv.deps.Logger.Info("WEB", "Cloudflare推送成功: %d 个IP (端口443)", cfPushCount)
			}
		}
	} else {
		if cfg.CFPushCount <= 0 {
			job.addLog("Cloudflare 推送已禁用 (cf_push_count=0)，跳过")
			srv.deps.Logger.Info("WEB", "Cloudflare推送跳过: cf_push_count=0")
		} else {
			job.addLog("Cloudflare 配置不完整，跳过 DNS 推送")
		}
	}

	// 8. GitHub 推送
	ghOK := false
	if cfg.GithubPushCount > 0 && cfg.GithubToken != "" && cfg.GithubRepo != "" {
		pushCount := cfg.GithubPushCount
		if pushCount > len(topResults) {
			pushCount = len(topResults)
		}
		job.setMessage("推送 GitHub 中...")
		ghPusher := pusher.NewGithubPusher(cfg)
		if err := ghPusher.Push(topResults[:pushCount]); err != nil {
			job.addLog("GitHub 推送失败: %v", err)
			srv.deps.Logger.Error("WEB", "GitHub 推送失败: %v", err)
		} else {
			ghOK = true
			job.addLog("GitHub 推送成功")
		}
	} else if cfg.GithubPushCount > 0 {
		job.addLog("GitHub 配置不完整，跳过")
	}

	// 9. 完成
	job.setProgress(total, total)
	parts := []string{}
	if cfg.CFAPIKey != "" && cfg.CFZoneID != "" && cfg.CFDNSName != "" {
		if cfOK {
			parts = append(parts, "Cloudflare:成功")
		} else {
			parts = append(parts, "Cloudflare:失败")
		}
	}
	if cfg.GithubPushCount > 0 && cfg.GithubToken != "" && cfg.GithubRepo != "" {
		if ghOK {
			parts = append(parts, "GitHub:成功")
		} else {
			parts = append(parts, "GitHub:失败")
		}
	}
	if len(parts) == 0 {
		job.setMessage("推送完成（无推送目标）")
	} else {
		job.setMessage("推送完成: %s", joinStr(parts, ", "))
	}
	srv.deps.Logger.Info("WEB", "推送完成: 重测 %d, 入库 %d, 取前 %d", len(reResults), len(scoredResults), topN)
}

// finishJob 标记任务结束
func (srv *Server) finishJob(job *Job) {
	now := time.Now()
	job.mu.Lock()
	if job.Status == statusRunning {
		job.Status = statusDone
	}
	job.FinishedAt = &now
	msg := job.Message
	status := job.Status
	job.mu.Unlock()

	// P2-8: WxPusher 微信通知（仅在启用时发送）
	if srv.deps.Cfg.WxPusherEnable {
		wxPusher := pusher.NewWxPusher(srv.deps.Cfg)
		success := status == statusDone
		if err := wxPusher.NotifyJobComplete(job.Type, success, msg); err != nil {
			srv.deps.Logger.Warn("WEB", "WxPusher 通知发送失败: %v", err)
		} else {
			srv.deps.Logger.Info("WEB", "WxPusher 通知已发送: 任务=%s 状态=%s", job.Type, status)
		}
	}
}

func joinStr(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
