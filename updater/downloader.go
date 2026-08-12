package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"time"

	"cf-speedtest/log"
)

// ProgressCallback 下载进度回调函数
//   - downloaded: 已下载字节数(含历史续传部分)
//   - total: 文件总大小(预期)
//   - speed: 当前速度(bytes/sec)
//   - eta: 预计剩余时间(下载完成时为 0)
type ProgressCallback func(downloaded, total int64, speed float64, eta time.Duration)

// Downloader 下载器
// 支持:断点续传(HTTP Range)、进度回调、ctx 取消、失败重试、sha256+size 校验
type Downloader struct {
	httpClient *http.Client // HTTP 客户端(Timeout=0,由 ctx 控制)
	tempDir    string       // 临时目录(空=os.TempDir())
	logger     *log.Logger  // 日志记录器(可为 nil)
}

// NewDownloader 创建下载器
// tempDir 为空时使用 os.TempDir()
// proxy 为 HTTP 代理地址(http://host:port,空=直连)
// logger 用于记录下载流程的关键事件(开始/续传/重试/完成/校验),可为 nil
func NewDownloader(tempDir, proxy string, logger *log.Logger) *Downloader {
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	client := &http.Client{Timeout: 0}
	if proxy != "" {
		if pu, err := neturl.Parse(proxy); err == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
			if logger != nil {
				logger.Info("UPDATE", "下载器已配置代理: %s", maskProxy(proxy))
			}
		}
	}
	return &Downloader{
		// Timeout=0:下载大文件时不因总耗时超时,由 ctx 控制
		httpClient: client,
		tempDir:    tempDir,
		logger:     logger,
	}
}

// DownloadResult 下载完成后的结果(用于后续解压/安装)
type DownloadResult struct {
	Path   string // 下载文件的完整路径
	Size   int64  // 实际下载字节数
	SHA256 string // 计算得到的 sha256(小写十六进制)
}

// Download 下载文件到临时目录,支持断点续传 + 进度回调 + ctx 取消
// 流程:
//  1. 检查临时文件是否已存在(断点续传起点)
//  2. 发送 HTTP Range 请求
//  3. 根据 206(续传)/200(从头)决定写入方式
//  4. 每 progressInterval 推送一次进度
//  5. 失败重试(最多 maxRetries 次,每次从已下载大小续传)
//
// 仅下载,不校验。校验由 Verify() 完成(便于 Manager 切换 state=verifying)
func (d *Downloader) Download(ctx context.Context, url, filename string, expectedSize int64, onProgress ProgressCallback) (string, error) {
	if err := os.MkdirAll(d.tempDir, 0755); err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	destPath := filepath.Join(d.tempDir, filename)

	d.logInfo("下载开始: %s (预期大小: %d 字节, 临时目录: %s)",
		filename, expectedSize, d.tempDir)
	downloadStart := time.Now()

	// 进度推送频率:500ms 一次,避免过于频繁影响下载性能
	progressInterval := 500 * time.Millisecond

	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// 检查 ctx 是否已取消(重试前)
		if err := ctx.Err(); err != nil {
			d.logInfo("下载取消(ctx): %v", err)
			return destPath, err
		}

		// 当前已下载大小(用于断点续传)
		var startByte int64
		if info, err := os.Stat(destPath); err == nil {
			startByte = info.Size()
			if startByte > 0 {
				d.logInfo("检测到断点续传: 已下载 %d 字节 (attempt %d/%d)",
					startByte, attempt, maxRetries)
			}
			// 已下载完成:无需再下载
			if expectedSize > 0 && startByte >= expectedSize {
				d.logInfo("临时文件已完整 (size=%d),跳过下载", startByte)
				if onProgress != nil {
					onProgress(startByte, expectedSize, 0, 0)
				}
				return destPath, nil
			}
		}

		err := d.downloadRange(ctx, url, destPath, startByte, expectedSize, onProgress, progressInterval)
		if err == nil {
			d.logInfo("下载完成: %s (耗时 %s)",
				destPath, time.Since(downloadStart).Round(time.Millisecond))
			return destPath, nil
		}

		// ctx 取消:立即返回,不再重试
		if ctx.Err() != nil {
			d.logInfo("下载取消(ctx): %v", err)
			return destPath, ctx.Err()
		}

		lastErr = err
		if attempt < maxRetries {
			d.logWarn("下载第 %d/%d 次尝试失败: %v (将重试)", attempt, maxRetries, err)
		} else {
			d.logWarn("下载第 %d/%d 次尝试失败: %v (已达最大重试次数)", attempt, maxRetries, err)
		}
		// 下次重试时,从已下载大小续传(downloadRange 内部已更新 destPath)
	}
	return destPath, fmt.Errorf("下载失败(已重试 %d 次): %w", maxRetries, lastErr)
}

// downloadRange 执行单次 HTTP Range 下载
// startByte: 已下载字节数(续传起点)
func (d *Downloader) downloadRange(ctx context.Context, url, destPath string, startByte, expectedSize int64, onProgress ProgressCallback, progressInterval time.Duration) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if startByte > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startByte))
	}
	// Accept 压缩会破坏 Range 续传(content-length 会变),显式禁用
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 根据状态码决定打开方式
	var f *os.File
	switch resp.StatusCode {
	case http.StatusOK:
		// 服务器不支持续传(或 startByte=0):从头写入
		if startByte > 0 {
			d.logInfo("服务器不支持续传或文件已变化,从头下载 (HTTP 200, startByte=%d → 0)", startByte)
		}
		startByte = 0
		f, err = os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	case http.StatusPartialContent:
		// 续传:追加写入
		if startByte > 0 {
			d.logInfo("续传下载: 从字节 %d 开始 (HTTP 206, Content-Length=%d)", startByte, resp.ContentLength)
		}
		f, err = os.OpenFile(destPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	default:
		return fmt.Errorf("下载失败: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	if err != nil {
		return fmt.Errorf("打开临时文件失败: %w", err)
	}
	defer f.Close()

	// 总大小:优先用 expectedSize(来自 version.json),否则用 Content-Length
	totalSize := expectedSize
	if totalSize <= 0 && resp.ContentLength > 0 {
		totalSize = startByte + resp.ContentLength
	}

	// 进度推送(每 progressInterval 一次)
	var ticker *time.Ticker
	var tickerC <-chan time.Time
	if onProgress != nil {
		// 首次推送,告知前端已开始下载
		onProgress(startByte, totalSize, 0, 0)
		ticker = time.NewTicker(progressInterval)
		tickerC = ticker.C
		defer ticker.Stop()
	}

	lastTime := time.Now()
	lastBytes := startByte
	currentBytes := startByte

	buf := make([]byte, 32*1024) // 32KB 缓冲
	for {
		// 检查 ctx 取消(在 Read 前检查,确保快速响应)
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fmt.Errorf("写入临时文件失败: %w", werr)
			}
			currentBytes += int64(n)

			// 推送进度(每 progressInterval 一次)
			if ticker != nil {
				select {
				case <-tickerC:
					now := time.Now()
					elapsed := now.Sub(lastTime).Seconds()
					if elapsed > 0 {
						deltaBytes := currentBytes - lastBytes
						speed := float64(deltaBytes) / elapsed
						var eta time.Duration
						if speed > 0 && totalSize > currentBytes {
							remaining := totalSize - currentBytes
							eta = time.Duration(float64(remaining)/speed) * time.Second
						}
						onProgress(currentBytes, totalSize, speed, eta)
						lastBytes = currentBytes
						lastTime = now
					}
				default:
					// ticker 未到时间,继续读
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	// 下载完成:推送 100% 进度
	if onProgress != nil {
		onProgress(currentBytes, totalSize, 0, 0)
	}
	return nil
}

// Verify 校验已下载文件的 sha256 和大小
// expectedSize<=0 跳过大小校验;expectedSHA256 为空跳过 sha256 校验
func (d *Downloader) Verify(path string, expectedSize int64, expectedSHA256 string) (*DownloadResult, error) {
	verifyStart := time.Now()
	d.logInfo("开始校验: %s (预期 size=%d, sha256=%s)",
		path, expectedSize, truncateSHA(expectedSHA256))

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取下载文件失败: %w", err)
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		d.logWarn("大小校验失败: 实际 %d, 预期 %d", info.Size(), expectedSize)
		return nil, fmt.Errorf("下载大小不匹配: 实际 %d, 预期 %d", info.Size(), expectedSize)
	}

	// 计算 sha256
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开下载文件失败: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("计算 sha256 失败: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if expectedSHA256 != "" && sum != expectedSHA256 {
		d.logWarn("sha256 校验失败: 实际 %s, 预期 %s", sum, expectedSHA256)
		return nil, fmt.Errorf("sha256 校验失败: 实际 %s, 预期 %s", sum, expectedSHA256)
	}

	d.logInfo("校验通过: size=%d, sha256=%s (耗时 %s)",
		info.Size(), truncateSHA(sum), time.Since(verifyStart).Round(time.Millisecond))

	return &DownloadResult{
		Path:   path,
		Size:   info.Size(),
		SHA256: sum,
	}, nil
}

// truncateSHA 截断 sha256 字符串到前 12 位 + "..." 便于日志显示
// 完整 sha256 仍会保留在错误信息中(用于诊断)
func truncateSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "..."
}

// Cleanup 删除临时下载文件(更新成功/失败后调用)
func (d *Downloader) Cleanup(path string) {
	if path == "" {
		return
	}
	// 忽略错误(文件可能已被删除或不存在)
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			d.logWarn("清理临时文件失败 %s: %v", path, err)
		}
	} else {
		d.logInfo("已清理临时下载文件: %s", path)
	}
}

// logInfo 记录 Info 级别日志(logger 为 nil 时跳过)
func (d *Downloader) logInfo(format string, args ...interface{}) {
	if d.logger != nil {
		d.logger.Info("UPDATE", format, args...)
	}
}

// logWarn 记录 Warn 级别日志(logger 为 nil 时跳过)
func (d *Downloader) logWarn(format string, args ...interface{}) {
	if d.logger != nil {
		d.logger.Warn("UPDATE", format, args...)
	}
}
