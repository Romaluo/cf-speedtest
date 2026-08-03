package geo

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ColoToCountry 默认 colo→国家代码映射表
var ColoToCountry = map[string]string{
	"NRT": "JP", // 东京成田
	"HND": "JP", // 东京羽田
	"KIX": "JP", // 大阪关西
	"HKG": "HK", // 香港
	"TPE": "TW", // 台北
	"KHH": "TW", // 高雄
	"ICN": "KR", // 首尔仁川
	"SJC": "US", // 圣何塞
	"LAX": "US", // 洛杉矶
	"SIN": "SG", // 新加坡
	"FRA": "DE", // 法兰克福
	"AMS": "NL", // 阿姆斯特丹
	"DEN": "US", // 丹佛
}

// TraceVerifier 通过 cdn-cgi/trace 接口验证 Cloudflare IP 的实际路由位置
type TraceVerifier struct {
	endpoint       string
	httpTimeout    time.Duration
	connectTimeout time.Duration
	skipTLSVerify  bool
	client         *http.Client
}

// NewTraceVerifier 创建 trace 验证器
func NewTraceVerifier(endpoint string, httpTimeout, connectTimeout time.Duration) *TraceVerifier {
	if endpoint == "" {
		endpoint = "https://www.cloudflare.com/cdn-cgi/trace"
	}
	if httpTimeout == 0 {
		httpTimeout = 8 * time.Second
	}
	if connectTimeout == 0 {
		connectTimeout = 4 * time.Second
	}
	return &TraceVerifier{
		endpoint:       endpoint,
		httpTimeout:    httpTimeout,
		connectTimeout: connectTimeout,
		skipTLSVerify:  true, // 验证用，跳过TLS验证
	}
}

// VerifyIP 验证指定 IP 的 Cloudflare 数据中心(colo)，返回 colo 代码和国家代码
func (tv *TraceVerifier) VerifyIP(ip string) (colo, countryCode string, err error) {
	// 从 endpoint URL 提取端口(默认 443)
	tracePort := "443"
	if u, e := url.Parse(tv.endpoint); e == nil && u.Port() != "" {
		tracePort = u.Port()
	}

	// 构造自定义 Transport：强制连接到指定 IP
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: tv.connectTimeout}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, tracePort))
		},
		TLSClientConfig: &tls.Config{
			ServerName:         "www.cloudflare.com",
			InsecureSkipVerify: tv.skipTLSVerify,
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   tv.httpTimeout,
	}

	resp, err := client.Get(tv.endpoint)
	if err != nil {
		return "", "", fmt.Errorf("trace请求失败 (IP: %s): %w", ip, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("trace HTTP %d (IP: %s)", resp.StatusCode, ip)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("读取trace响应失败: %w", err)
	}

	fields := parseTrace(string(body))
	colo = fields["colo"]
	if colo == "" {
		return "", "", fmt.Errorf("trace响应中未找到colo字段 (IP: %s)", ip)
	}

	countryCode = MapColoToCountry(colo)
	return colo, countryCode, nil
}

// MapColoToCountry 将 colo 代码映射为国家代码（大小写不敏感）
func MapColoToCountry(colo string) string {
	return ColoToCountry[strings.ToUpper(colo)]
}

// parseTrace 解析 cdn-cgi/trace 响应文本
func parseTrace(raw string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}
