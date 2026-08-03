package engine

import (
	"strings"

	"cf-speedtest/geo"
	"cf-speedtest/model"
	"cf-speedtest/scorer"
)

// FillCountryCodes 为没有国家代码的 Task 填充归属地信息
// 官方 CIDR 采样的 IP 不带国家代码，需通过 resolver 查询补全
// 自定义地址拉取的 IP 通常已带国家代码（如 #JP），无需查询
// 查询优先级：纠错覆盖层 → xdb 数据库 → (可选)cdn-cgi/trace 精准验证
func FillCountryCodes(tasks []model.Task, resolver *geo.Resolver, useTrace bool) {
	if resolver == nil || !resolver.IsEnabled() {
		return
	}
	for i := range tasks {
		if tasks[i].CountryCode != "" {
			continue
		}
		info := resolver.LookupWithTrace(tasks[i].IP, useTrace)
		if info == nil {
			continue
		}
		// 优先使用 CountryCode 字段（如 "US"/"CN"）
		if info.CountryCode != "" && info.CountryCode != "0" {
			tasks[i].CountryCode = strings.ToUpper(info.CountryCode)
		} else if info.Country != "" && info.Country != "0" {
			// 退化方案：将国家中文名转换为 ISO 代码
			tasks[i].CountryCode = scorer.GetCountryCode(info.Country)
		}
	}
}

// FilterTasksByCountries 按国家代码过滤任务列表（测速前预过滤）
// 用于手动模式下，仅保留指定国家的 IP 进入测速流程
func FilterTasksByCountries(tasks []model.Task, countries []string) []model.Task {
	if len(countries) == 0 || len(tasks) == 0 {
		return tasks
	}
	set := make(map[string]bool, len(countries))
	for _, c := range countries {
		set[strings.ToUpper(strings.TrimSpace(c))] = true
	}
	var filtered []model.Task
	for _, t := range tasks {
		if set[t.CountryCode] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
