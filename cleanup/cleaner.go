// Package cleanup 提供任务完成后的系统资源清理机制
// 包括：内存GC、临时文件清理、子进程终止、资源指标验证
package cleanup

import (
	"fmt"
	"runtime"
	"time"

	"cf-speedtest/config"
	"cf-speedtest/log"
)

// Cleaner 资源清理器，协调各项清理操作
type Cleaner struct {
	cfg    *config.Config
	logger *log.Logger
	tracker *ProcessTracker
}

// CleanupResult 清理结果
type CleanupResult struct {
	JobType          string        `json:"job_type"`
	Success          bool          `json:"success"`
	MemoryBefore     uint64        `json:"memory_before"`      // 任务前堆内存（字节）
	MemoryAfter      uint64        `json:"memory_after"`       // 清理后堆内存（字节）
	MemoryReleased   uint64        `json:"memory_released"`    // 释放的内存（字节）
	MemoryRatio      float64       `json:"memory_ratio"`       // 释放比例（0-1）
	TempFilesDeleted int           `json:"temp_files_deleted"` // 删除的临时文件数
	ProcessesKilled  int           `json:"processes_killed"`   // 终止的子进程数
	DBVacuumed       bool          `json:"db_vacuumed"`        // 是否执行了数据库压缩
	VerifyPassed     bool          `json:"verify_passed"`      // 资源指标是否回归基线
	Errors           []string      `json:"errors,omitempty"`
	Warnings         []string      `json:"warnings,omitempty"`
	Duration         time.Duration `json:"duration"`
}

// Vacuumer 数据库压缩接口（解耦 repository 包）
type Vacuumer interface {
	Vacuum() error
}

// New 创建清理器
func New(cfg *config.Config, logger *log.Logger) *Cleaner {
	return &Cleaner{
		cfg:     cfg,
		logger:  logger,
		tracker: NewProcessTracker(cfg.Cleanup.ProcessTimeout),
	}
}

// Tracker 返回进程跟踪器，供任务执行时注册子进程
func (c *Cleaner) Tracker() *ProcessTracker {
	return c.tracker
}

// Cleanup 执行指定任务类型的清理
// baseline: 任务开始前的资源快照（用于对比验证）
// vacuumer: 数据库压缩接口（DBVacuum 策略启用时使用，可为 nil）
func (c *Cleaner) Cleanup(jobType string, baseline *ResourceSnapshot, vacuumer Vacuumer) *CleanupResult {
	start := time.Now()
	result := &CleanupResult{JobType: jobType}

	if !c.cfg.Cleanup.Enable {
		result.Success = true
		result.Warnings = append(result.Warnings, "清理机制未启用")
		return result
	}

	strategy, ok := c.cfg.Cleanup.Strategies[jobType]
	if !ok {
		// 未知任务类型，使用默认全量策略
		strategy = config.CleanupStrategy{GC: true, TempFiles: true, Processes: true, Verify: true}
		result.Warnings = append(result.Warnings, fmt.Sprintf("任务类型 '%s' 无明确策略，使用默认策略", jobType))
	}

	c.logger.Info("CLEANUP", "开始清理 [%s] 任务资源", jobType)

	// 记录清理前内存
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	result.MemoryBefore = memBefore.HeapAlloc

	// 1. 终止残留子进程
	if strategy.Processes {
		killed, errs := c.tracker.KillAll()
		result.ProcessesKilled = killed
		for _, err := range errs {
			result.Errors = append(result.Errors, fmt.Sprintf("进程终止: %v", err))
		}
		if killed > 0 {
			c.logger.Info("CLEANUP", "终止 %d 个残留子进程", killed)
		}
	}

	// 2. 清理临时文件
	if strategy.TempFiles {
		deleted, errs := CleanTempFiles(c.cfg.Cleanup.TempFiles, c.cfg.Cleanup.TempPatterns)
		result.TempFilesDeleted = deleted
		for _, err := range errs {
			result.Errors = append(result.Errors, fmt.Sprintf("临时文件: %v", err))
		}
		if deleted > 0 {
			c.logger.Info("CLEANUP", "删除 %d 个临时文件", deleted)
		}
	}

	// 3. 数据库 VACUUM 压缩
	if strategy.DBVacuum && vacuumer != nil {
		if err := vacuumer.Vacuum(); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("数据库压缩: %v", err))
			c.logger.Warn("CLEANUP", "数据库 VACUUM 失败: %v", err)
		} else {
			result.DBVacuumed = true
			c.logger.Info("CLEANUP", "数据库 VACUUM 完成")
		}
	}

	// 4. 触发垃圾回收释放内存
	if strategy.GC {
		runtime.GC()
		runtime.GC() // 二次 GC 确保内存回收
		c.logger.Debug("CLEANUP", "已触发垃圾回收")
	}

	// 记录清理后内存
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	result.MemoryAfter = memAfter.HeapAlloc

	// 计算内存释放比例
	if baseline != nil {
		// 使用基线快照对比任务前后的额外内存占用
		extraMemory := int64(memAfter.HeapAlloc) - int64(baseline.HeapAlloc)
		if extraMemory > 0 {
			// 仍有额外内存占用，计算已释放比例
			peakExtra := int64(memBefore.HeapAlloc) - int64(baseline.HeapAlloc)
			if peakExtra > 0 {
				result.MemoryReleased = uint64(peakExtra - extraMemory)
				result.MemoryRatio = float64(result.MemoryReleased) / float64(peakExtra)
			}
		} else {
			// 内存已完全回归基线
			peakExtra := int64(memBefore.HeapAlloc) - int64(baseline.HeapAlloc)
			if peakExtra > 0 {
				result.MemoryReleased = uint64(peakExtra)
				result.MemoryRatio = 1.0
			}
		}
	} else {
		// 无基线，使用清理前后对比
		if memBefore.HeapAlloc > memAfter.HeapAlloc {
			result.MemoryReleased = memBefore.HeapAlloc - memAfter.HeapAlloc
			result.MemoryRatio = float64(result.MemoryReleased) / float64(memBefore.HeapAlloc)
		}
	}

	// 5. 验证资源指标回归基线
	if strategy.Verify && c.cfg.Cleanup.VerifyResources && baseline != nil {
		afterSnapshot := Snapshot()
		verifyErrs := VerifyResources(baseline, afterSnapshot, c.cfg.Cleanup.MemoryThreshold)
		if len(verifyErrs) == 0 {
			result.VerifyPassed = true
			c.logger.Info("CLEANUP", "资源验证通过：CPU/内存/磁盘指标已回归基线")
		} else {
			result.VerifyPassed = false
			for _, err := range verifyErrs {
				result.Warnings = append(result.Warnings, err.Error())
			}
			c.logger.Warn("CLEANUP", "资源验证未通过: %v", verifyErrs)
		}
	} else if !baseline.IsZero() {
		result.VerifyPassed = true
	}

	result.Duration = time.Since(start)
	result.Success = len(result.Errors) == 0

	// 清理结果日志
	if result.Success {
		c.logger.Info("CLEANUP", "清理完成 [%s]: 内存释放 %s (%.1f%%), 临时文件 %d, 进程 %d, 耗时 %s",
			jobType,
			formatBytes(result.MemoryReleased),
			result.MemoryRatio*100,
			result.TempFilesDeleted,
			result.ProcessesKilled,
			result.Duration.Round(time.Millisecond))
	} else {
		c.logger.Warn("CLEANUP", "清理完成（有错误）[%s]: %d 个错误, 耗时 %s",
			jobType, len(result.Errors), result.Duration.Round(time.Millisecond))
		for _, err := range result.Errors {
			c.logger.Error("CLEANUP", "  错误: %s", err)
		}
	}

	return result
}

// formatBytes 格式化字节数为人类可读字符串
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
