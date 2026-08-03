package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CleanTempFiles 清理临时文件
// files: 固定路径文件列表
// patterns: 通配符模式列表（如 /tmp/cf-speedtest-*）
// 返回删除的文件数和错误列表
func CleanTempFiles(files []string, patterns []string) (int, []error) {
	var errs []error
	deleted := 0

	// 1. 清理固定路径文件
	for _, f := range files {
		if f == "" {
			continue
		}
		// 跳过关键文件（数据库、配置、日志）
		if isProtectedFile(f) {
			continue
		}
		err := os.Remove(f)
		if err == nil {
			deleted++
		} else if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除 %s 失败: %w", f, err))
		}
	}

	// 2. 清理通配符匹配的文件
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("匹配模式 %s 失败: %w", pattern, err))
			continue
		}
		for _, m := range matches {
			if isProtectedFile(m) {
				continue
			}
			err := os.Remove(m)
			if err == nil {
				deleted++
			} else if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("删除 %s 失败: %w", m, err))
			}
		}
	}

	return deleted, errs
}

// isProtectedFile 判断是否为受保护的关键文件（不可删除）
func isProtectedFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	// 受保护文件列表
	protected := []string{
		"config.yaml", "config.yml",
		"speedtest.db", "speedtest.db-wal", "speedtest.db-shm",
		"ip2region.xdb",
		"cf-speedtest.log", "cf-speedtest.log.1",
		"go.mod", "go.sum",
	}
	for _, p := range protected {
		if base == p {
			return true
		}
	}
	// 保护所有 .go 源文件
	if strings.HasSuffix(base, ".go") {
		return true
	}
	return false
}
