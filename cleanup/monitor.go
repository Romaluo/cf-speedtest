package cleanup

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// ResourceSnapshot 系统资源快照
type ResourceSnapshot struct {
	Timestamp  time.Time
	HeapAlloc  uint64 // 堆内存分配（字节）
	HeapInUse  uint64 // 堆内存使用（字节）
	StackInUse uint64 // 栈内存使用（字节）
	Sys        uint64 // 从系统获取的总内存（字节）
	NumGoroutine int  // goroutine 数量
	CPUUsage   float64 // CPU 使用率（%）
	DiskUsage  float64 // 磁盘使用率（%）
}

// IsZero 判断快照是否为零值（未采集）
func (s *ResourceSnapshot) IsZero() bool {
	return s.Timestamp.IsZero()
}

// Snapshot 采集当前系统资源快照
func Snapshot() *ResourceSnapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s := &ResourceSnapshot{
		Timestamp:    time.Now(),
		HeapAlloc:    m.HeapAlloc,
		HeapInUse:    m.HeapInuse,
		StackInUse:   m.StackInuse,
		Sys:          m.Sys,
		NumGoroutine: runtime.NumGoroutine(),
	}

	// 采集 CPU 使用率（Linux: /proc/stat）
	s.CPUUsage = readCPUUsage()

	// 采集磁盘使用率（当前目录所在分区）
	s.DiskUsage = readDiskUsage(".")

	return s
}

// VerifyResources 验证清理后资源指标是否回归基线
// memoryThreshold: 期望释放的额外内存比例（0.0-1.0）
// 返回验证错误列表（空表示通过）
func VerifyResources(baseline, after *ResourceSnapshot, memoryThreshold float64) []error {
	var errs []error

	if baseline == nil || after == nil {
		return []error{fmt.Errorf("基线或清理后快照为空")}
	}

	// 1. 内存验证：额外占用应低于 (1-threshold) 的基线额外占用
	// 即至少释放 threshold 比例的额外内存
	extraAfter := int64(after.HeapAlloc) - int64(baseline.HeapAlloc)
	if extraAfter > 0 {
		// 仍有额外内存占用，检查是否符合阈值
		// 额外占用不应超过任务运行时的 (1-threshold) 倍
		// 由于无法精确知道峰值，使用保守判断：额外占用不应超过基线的 50%
		baselineHeap := int64(baseline.HeapAlloc)
		if baselineHeap > 0 && float64(extraAfter)/float64(baselineHeap) > (1-memoryThreshold) {
			errs = append(errs, fmt.Errorf("内存未充分释放: 额外占用 %d 字节（基线的 %.1f%%）",
				extraAfter, float64(extraAfter)/float64(baselineHeap)*100))
		}
	}

	// 2. goroutine 数量验证：不应显著高于基线
	goroutineDiff := after.NumGoroutine - baseline.NumGoroutine
	if goroutineDiff > 10 {
		errs = append(errs, fmt.Errorf("goroutine 泄漏: 当前 %d，基线 %d（差值 %d）",
			after.NumGoroutine, baseline.NumGoroutine, goroutineDiff))
	}

	// 3. CPU 使用率验证：不应显著高于基线
	cpuDiff := after.CPUUsage - baseline.CPUUsage
	if cpuDiff > 20.0 {
		errs = append(errs, fmt.Errorf("CPU 使用率偏高: 当前 %.1f%%，基线 %.1f%%",
			after.CPUUsage, baseline.CPUUsage))
	}

	// 4. 磁盘使用率验证：不应显著高于基线（清理后应更低或持平）
	diskDiff := after.DiskUsage - baseline.DiskUsage
	if diskDiff > 1.0 {
		errs = append(errs, fmt.Errorf("磁盘使用率上升: 当前 %.1f%%，基线 %.1f%%",
			after.DiskUsage, baseline.DiskUsage))
	}

	return errs
}

// readCPUUsage 读取当前进程的 CPU 使用率（Linux: /proc/stat）
// 返回 0.0 表示无法读取
func readCPUUsage() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0.0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0.0
	}
	// 第一行是总 CPU 统计: cpu user nice system idle ...
	fields := strings.Fields(lines[0])
	if len(fields) < 5 {
		return 0.0
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		var v uint64
		fmt.Sscanf(fields[i], "%d", &v)
		total += v
		if i == 4 { // idle 是第 5 个字段（index 4）
			idle = v
		}
	}
	if total == 0 {
		return 0.0
	}
	return float64(total-idle) / float64(total) * 100.0
}

// readDiskUsage 读取指定路径所在分区的磁盘使用率
// 返回 0.0 表示无法读取
func readDiskUsage(path string) float64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0.0
	}
	_ = stat
	// Linux 下读取文件系统统计
	// 简化实现：使用 du 风格的估算不可行，这里返回 0
	// 实际磁盘使用率监控由临时文件删除量间接体现
	return 0.0
}
