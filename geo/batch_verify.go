package geo

import (
	"sync"

	"cf-speedtest/model"
)

// VerifyTasksCountryCodes 对测速任务批量执行国家代码纠错验证
// 在国家过滤后、测速前调用，确保进入测速的 IP 国家代码准确
// 已在纠错层中的 IP 直接修正（无需再次 trace）
// 验证失败的 IP（trace 超时/错误）直接从结果中移除，不进入测速
// 返回：验证通过的任务列表，修正的数量，失败（移除）的数量
func VerifyTasksCountryCodes(tasks []model.Task, resolver *Resolver, concurrency int) (validTasks []model.Task, corrected, failed int) {
	if resolver == nil || !resolver.IsEnabled() || resolver.traceVerifier == nil {
		return tasks, 0, 0
	}
	if concurrency <= 0 {
		concurrency = 10
	}

	var mu sync.Mutex
	passed := make([]bool, len(tasks))

	// 第一阶段:已在纠错层中的 IP 直接修正,无需 trace,无需 goroutine
	needVerify := make([]int, 0, len(tasks))
	for i := range tasks {
		if resolver.corrections != nil {
			if cc, ok := resolver.corrections.Lookup(tasks[i].IP); ok {
				if tasks[i].CountryCode != cc {
					tasks[i].CountryCode = cc
					corrected++
				}
				passed[i] = true
				continue
			}
		}
		needVerify = append(needVerify, i)
	}

	if len(needVerify) > 0 {
		// 第二阶段:worker pool 仅启动 concurrency 个 worker 处理需要 trace 的任务
		// 内存优化:避免为每个待验证 IP 创建一个 goroutine
		taskCh := make(chan int, concurrency)
		var wg sync.WaitGroup
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range taskCh {
					colo, cc, err := resolver.traceVerifier.VerifyIP(tasks[idx].IP)
					if err != nil || cc == "" {
						mu.Lock()
						failed++
						mu.Unlock()
						continue
					}

					oldCC := tasks[idx].CountryCode
					tasks[idx].CountryCode = cc

					if resolver.corrections != nil {
						resolver.corrections.Add(tasks[idx].IP, cc)
					}

					if oldCC != cc {
						mu.Lock()
						corrected++
						mu.Unlock()
					}
					passed[idx] = true
					_ = colo
				}
			}()
		}
		for _, idx := range needVerify {
			taskCh <- idx
		}
		close(taskCh)
		wg.Wait()
	}

	// 只保留验证通过的 IP（失败的直接移除，不进入测速）
	validTasks = make([]model.Task, 0, len(tasks))
	for i, t := range tasks {
		if passed[i] {
			validTasks = append(validTasks, t)
		}
	}

	return validTasks, corrected, failed
}
