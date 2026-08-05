package engine

import (
	"fmt"
	"net"
	"sync"
	"time"

	"cf-speedtest/model"
)

// PreFilterResult TCP 握手预筛选结果
type PreFilterResult struct {
	IP      string        `json:"ip"`
	Port    int           `json:"port"`
	Success bool          `json:"success"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
}

// PreFilterStats 预筛选统计
type PreFilterStats struct {
	Total      int           `json:"total"`
	Success    int           `json:"success"`
	Failed     int           `json:"failed"`
	Duration   time.Duration `json:"duration"`
	AvgLatency time.Duration `json:"avg_latency"`
}

// PreFilterTCP 对任务列表执行并发 TCP 握手预筛选
// 仅执行单次握手，快速过滤不可达的 IP:Port
// 返回每个任务的结果和汇总统计
func PreFilterTCP(tasks []model.Task, concurrency int, timeout time.Duration) ([]PreFilterResult, PreFilterStats) {
	if len(tasks) == 0 {
		return nil, PreFilterStats{}
	}

	if concurrency <= 0 {
		concurrency = 100
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	results := make([]PreFilterResult, len(tasks))
	var successCount int64
	var totalLatency int64
	var mu sync.Mutex

	start := time.Now()

	// 内存优化:worker pool 模式 — 仅启动 concurrency 个 worker,而非为每个任务创建 goroutine
	// 避免 10 万任务 = 10 万 goroutine(约 800MB 栈内存)的爆炸
	taskCh := make(chan int, concurrency)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range taskCh {
				t := tasks[idx]
				result := PreFilterResult{IP: t.IP, Port: t.Port}
				handshakeStart := time.Now()
				addr := net.JoinHostPort(t.IP, fmt.Sprintf("%d", t.Port))
				conn, err := net.DialTimeout("tcp", addr, timeout)
				result.Latency = time.Since(handshakeStart)

				if err != nil {
					result.Success = false
					result.Error = err.Error()
				} else {
					result.Success = true
					conn.Close()
					mu.Lock()
					successCount++
					totalLatency += int64(result.Latency)
					mu.Unlock()
				}

				results[idx] = result
			}
		}()
	}

	for i := range tasks {
		taskCh <- i
	}
	close(taskCh)
	wg.Wait()

	stats := PreFilterStats{
		Total:    len(tasks),
		Success:  int(successCount),
		Failed:   len(tasks) - int(successCount),
		Duration: time.Since(start),
	}
	if successCount > 0 {
		stats.AvgLatency = time.Duration(totalLatency / successCount)
	}

	return results, stats
}

// FilterReachable 从预筛选结果中提取可达的 IP:Port 任务列表
func FilterReachable(tasks []model.Task, results []PreFilterResult) []model.Task {
	if len(tasks) != len(results) {
		return tasks
	}
	var reachable []model.Task
	for i, r := range results {
		if r.Success {
			reachable = append(reachable, tasks[i])
		}
	}
	return reachable
}
