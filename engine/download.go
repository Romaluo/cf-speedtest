package engine

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

// DownloadResult 下载测试结果
type DownloadResult struct {
	SpeedMBps float64 // 下载速度 MB/s（基于纯传输时间，排除握手）
}

// DownloadTest 测试下载带宽，绑定到指定 IP
// targetURL: 下载目标基础 URL（不含 bytes 参数，由 expectedSize 自动拼接）
// ip: 要测试的 Cloudflare IP 地址
// port: 目标端口
// expectedSize: 预期下载字节数，自动拼接到 URL 的 bytes 参数
//
// P0-2: 使用 httptrace.GotFirstResponseByte 获取纯传输时间（排除 TCP/TLS 握手与首字节等待），
//
//	并增加下载完整性校验（实际字节数与 expectedSize 匹配）。
//
// P2-7: connectTimeout 控制 TCP/TLS 握手，readTimeout 控制整体读取超时
//
//	任一为 0 时退化为使用另一值；二者均为 0 时使用 30s 默认值
func DownloadTest(targetURL string, ip string, port int, expectedSize int64, connectTimeout, readTimeout time.Duration) (*DownloadResult, error) {
	if connectTimeout <= 0 && readTimeout <= 0 {
		connectTimeout = 30 * time.Second
		readTimeout = 30 * time.Second
	} else if connectTimeout <= 0 {
		connectTimeout = readTimeout
	} else if readTimeout <= 0 {
		readTimeout = connectTimeout
	}

	// 如果 URL 中未包含 bytes 参数，则根据 expectedSize 自动拼接
	if expectedSize > 0 && !strings.Contains(targetURL, "bytes=") {
		sep := "?"
		if strings.Contains(targetURL, "?") {
			sep = "&"
		}
		targetURL = fmt.Sprintf("%s%sbytes=%d", targetURL, sep, expectedSize)
	}

	// 自定义 DialContext：强制连接到指定 IP，而不是 DNS 解析
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
		// 禁用连接复用，确保每次测试都是独立连接
		DisableKeepAlives: true,
	}
	// P2-10: 直连模式 — 显式禁用代理
	applyDirectProxy(transport)
	client := &http.Client{
		Timeout:   readTimeout,
		Transport: transport,
	}

	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request failed: %w", err)
	}

	// 通过 httptrace 捕获"收到首字节响应"的时间点
	var gotFirstByteAt time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			gotFirstByteAt = time.Now()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// 如果服务器未触发 GotFirstResponseByte（理论上不应发生），退化为当前时间
	transferStart := gotFirstByteAt
	if transferStart.IsZero() {
		transferStart = time.Now()
	}

	bytesRead, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download read failed: %w", err)
	}

	transferElapsed := time.Since(transferStart)
	if transferElapsed.Seconds() <= 0 {
		return nil, fmt.Errorf("invalid transfer elapsed time")
	}

	// 下载完整性校验：当 expectedSize 已知时，校验实际字节数
	if expectedSize > 0 && bytesRead != expectedSize {
		return nil, fmt.Errorf("download incomplete: expected %d bytes, got %d", expectedSize, bytesRead)
	}

	speedBytesPerSec := float64(bytesRead) / transferElapsed.Seconds()
	speedMBps := speedBytesPerSec / (1024 * 1024)

	return &DownloadResult{
		SpeedMBps: speedMBps,
	}, nil
}
