package engine

import (
	"net/http"
	"os"
	"sync"

	"cf-speedtest/config"
)

// directModeOnce 确保环境变量清除只执行一次
var directModeOnce sync.Once

// ApplyDirectMode 根据配置激活直连模式：
// 1. 清除进程内的 HTTP_PROXY / HTTPS_PROXY / ALL_PROXY 等环境变量
// 2. 使后续 http.Transport 显式禁用代理（applyDirectProxy 中实现）
//
// 应在程序启动早期、配置加载后调用一次。
func ApplyDirectMode(cfg *config.Config) {
	if !cfg.DirectModeEnable {
		return
	}
	directModeOnce.Do(func() {
		// 清除常见代理环境变量，防止 http.Transport 自动从环境读取代理
		for _, key := range []string{
			"HTTP_PROXY", "http_proxy",
			"HTTPS_PROXY", "https_proxy",
			"ALL_PROXY", "all_proxy",
			"NO_PROXY", "no_proxy",
		} {
			_ = os.Unsetenv(key)
		}
	})
}

// applyDirectProxy 在 http.Transport 上显式设置 Proxy: nil（无代理）
// 本项目的测速 transport 始终不使用系统代理，确保测速结果可控。
//
// 调用方应在构造 http.Transport 后立即调用此函数。
func applyDirectProxy(t *http.Transport) {
	if t == nil {
		return
	}
	t.Proxy = nil
}
