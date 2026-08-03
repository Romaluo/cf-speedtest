package engine

import (
	"fmt"
	"net"
	"time"
)

// TCPPingResult TCP Ping 结果
type TCPPingResult struct {
	AvgLatency time.Duration
	LossRate   float64
}

// TCPPing 对目标 IP:Port 执行多次 TCP 连接测试
func TCPPing(ip string, port int, count int, timeout time.Duration) (*TCPPingResult, error) {
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	var totalLatency time.Duration
	successCount := 0

	for i := 0; i < count; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			continue
		}
		latency := time.Since(start)
		conn.Close()

		totalLatency += latency
		successCount++
	}

	if successCount == 0 {
		return nil, fmt.Errorf("all %d TCP pings failed for %s", count, ip)
	}

	lossRate := float64(count-successCount) / float64(count)
	avgLatency := totalLatency / time.Duration(successCount)

	return &TCPPingResult{
		AvgLatency: avgLatency,
		LossRate:   lossRate,
	}, nil
}
