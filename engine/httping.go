package engine

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTPingResult HTTP 延迟测试结果
type HTTPingResult struct {
	AvgLatency time.Duration
	Jitter     time.Duration // 延迟标准差
	StatusCode int
}

// HTTPing 对目标 URL 执行多次 HTTP GET 请求，测量延迟（绑定到指定 IP）
// 同时实现 P0-1（抖动测量）和 P0-3（Server 头验证）
// P2-7: connectTimeout 控制 TCP/TLS 握手，readTimeout 控制 HTTP 总耗时
// 任一为 0 时退化为使用另一值；二者均为 0 时使用 5s 默认值
func HTTPing(targetURL string, ip string, port int, count int, connectTimeout, readTimeout time.Duration) (*HTTPingResult, error) {
	if connectTimeout <= 0 && readTimeout <= 0 {
		connectTimeout = 5 * time.Second
		readTimeout = 5 * time.Second
	} else if connectTimeout <= 0 {
		connectTimeout = readTimeout
	} else if readTimeout <= 0 {
		readTimeout = connectTimeout
	}

	dialer := &net.Dialer{
		Timeout: connectTimeout,
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			targetAddr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
			return dialer.DialContext(ctx, "tcp", targetAddr)
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout: connectTimeout,
		DisableKeepAlives:   true,
	}
	// P2-10: 直连模式 — 显式禁用代理
	applyDirectProxy(transport)
	client := &http.Client{
		Timeout:   readTimeout,
		Transport: transport,
	}
	// 内存优化:函数退出时清理 transport 持有的空闲连接与底层资源
	defer transport.CloseIdleConnections()

	var latencies []time.Duration
	var lastStatusCode int

	for i := 0; i < count; i++ {
		start := time.Now()
		resp, err := client.Get(targetURL)
		if err != nil {
			continue
		}
		latency := time.Since(start)

		// P0-3: HTTP Server 头验证（确认响应来自 Cloudflare）
		server := resp.Header.Get("Server")
		if server == "" || !strings.HasPrefix(strings.ToLower(server), "cloudflare") {
			resp.Body.Close()
			continue
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		latencies = append(latencies, latency)
		lastStatusCode = resp.StatusCode
	}

	if len(latencies) == 0 {
		return nil, fmt.Errorf("all %d HTTP requests failed (no valid cloudflare response)", count)
	}

	avgLatency, jitter := computeAvgAndJitter(latencies)
	return &HTTPingResult{
		AvgLatency: avgLatency,
		Jitter:     jitter,
		StatusCode: lastStatusCode,
	}, nil
}

// computeAvgAndJitter 计算延迟平均值与标准差（抖动）
func computeAvgAndJitter(latencies []time.Duration) (time.Duration, time.Duration) {
	if len(latencies) == 0 {
		return 0, 0
	}
	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	avg := total / time.Duration(len(latencies))

	if len(latencies) < 2 {
		return avg, 0
	}
	// 标准差：sqrt( Σ(x-avg)² / n )
	var sumSq float64
	avgNs := float64(avg.Nanoseconds())
	for _, l := range latencies {
		diff := float64(l.Nanoseconds()) - avgNs
		sumSq += diff * diff
	}
	stdDev := math.Sqrt(sumSq / float64(len(latencies)))
	return avg, time.Duration(stdDev)
}
