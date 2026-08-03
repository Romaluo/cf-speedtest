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
	wg := &sync.WaitGroup{}
	sem := make(chan struct{}, concurrency)

	// 标记每个 IP 是否验证通过
	passed := make([]bool, len(tasks))

	for i := range tasks {
		ip := tasks[i].IP

		// 1. 已在纠错层中的 IP 直接修正（无需 trace）
		if resolver.corrections != nil {
			if cc, ok := resolver.corrections.Lookup(ip); ok {
				if tasks[i].CountryCode != cc {
					tasks[i].CountryCode = cc
					corrected++
				}
				passed[i] = true
				continue
			}
		}

		// 2. 需要 trace 验证的 IP
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			colo, cc, err := resolver.traceVerifier.VerifyIP(tasks[idx].IP)
			if err != nil || cc == "" {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}

			oldCC := tasks[idx].CountryCode
			tasks[idx].CountryCode = cc

			// 持久化到纠错层
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
		}(i)
	}
	wg.Wait()

	// 只保留验证通过的 IP（失败的直接移除，不进入测速）
	validTasks = make([]model.Task, 0, len(tasks))
	for i, t := range tasks {
		if passed[i] {
			validTasks = append(validTasks, t)
		}
	}

	return validTasks, corrected, failed
}
