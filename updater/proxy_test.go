package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cf-speedtest/log"
)

// testProxyServer 简单 HTTP 正向代理,转发请求并记录请求数
type testProxyServer struct {
	listener   net.Listener
	server     *http.Server
	requestCnt int
	mu         sync.Mutex
}

// newTestProxy 启动一个本地 HTTP 正向代理
func newTestProxy(t *testing.T) *testProxyServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建 proxy listener 失败: %v", err)
	}
	p := &testProxyServer{listener: ln}
	p.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			p.requestCnt++
			p.mu.Unlock()

			// 正向代理:转发请求到目标服务器
			resp, err := http.DefaultTransport.RoundTrip(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		}),
	}
	go p.server.Serve(ln)
	return p
}

func (p *testProxyServer) URL() string {
	return "http://" + p.listener.Addr().String()
}

func (p *testProxyServer) RequestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requestCnt
}

func (p *testProxyServer) Close() {
	p.server.Close()
}

// TestProxyDownloader 验证 Downloader 通过代理下载文件
// 模拟:文件服务器 → 代理 → Downloader → 下载到临时目录 → 校验内容
func TestProxyDownloader(t *testing.T) {
	// 准备测试文件(约 24KB,模拟小型更新包)
	fileContent := bytes.Repeat([]byte("cf-speedtest-update-test-"), 1000)
	expectedSHA := sha256.Sum256(fileContent)
	expectedSHAHex := hex.EncodeToString(expectedSHA[:])

	// 1. 启动文件服务器(模拟 GitHub releases 托管的更新包)
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	// 2. 启动代理服务器
	proxy := newTestProxy(t)
	defer proxy.Close()

	// 3. 创建带代理的 Downloader
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	dl := NewDownloader(t.TempDir(), proxy.URL(), logger)

	// 4. 通过代理下载文件
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filename := "test-update-package.tar.gz"
	downloadPath, err := dl.Download(ctx, fileServer.URL+"/"+filename, filename, int64(len(fileContent)),
		func(downloaded, total int64, speed float64, eta time.Duration) {
			// 进度回调,不做断言(确保不阻塞)
		},
	)
	if err != nil {
		t.Fatalf("通过代理下载失败: %v", err)
	}

	// 5. 验证代理被使用(请求计数 > 0)
	if proxy.RequestCount() == 0 {
		t.Error("代理未被使用:请求数为 0,说明 Downloader 没有通过代理下载")
	}

	// 6. 验证下载文件存在
	info, err := os.Stat(downloadPath)
	if err != nil {
		t.Fatalf("下载文件不存在: %v", err)
	}
	if info.Size() != int64(len(fileContent)) {
		t.Errorf("文件大小不匹配: 期望 %d, 实际 %d", len(fileContent), info.Size())
	}

	// 7. 验证下载内容完整
	downloaded, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatalf("读取下载文件失败: %v", err)
	}
	if !bytes.Equal(downloaded, fileContent) {
		t.Errorf("下载内容不匹配: 期望 %d 字节, 实际 %d 字节", len(fileContent), len(downloaded))
	}

	// 8. 验证 sha256
	actualSHA := sha256.Sum256(downloaded)
	actualSHAHex := hex.EncodeToString(actualSHA[:])
	if actualSHAHex != expectedSHAHex {
		t.Errorf("sha256 不匹配:\n期望: %s\n实际: %s", expectedSHAHex, actualSHAHex)
	}

	t.Logf("✓ 代理下载成功: %d 字节, 代理请求数: %d, sha256: %s...",
		len(downloaded), proxy.RequestCount(), actualSHAHex[:16])
}

// TestProxyChecker 验证 Checker 通过代理拉取 version.json
// 模拟:version.json 服务器 → 代理 → Checker → 解析 manifest
func TestProxyChecker(t *testing.T) {
	// 准备 version.json
	manifest := VersionManifest{
		Version:             "1.2.0",
		ReleaseNotes:        "测试版本:修复代理下载问题",
		PublishedAt:        time.Now(),
		MinRequiredVersion: "1.0.0",
		Assets: map[string]PlatformAsset{
			"linux/amd64": {
				URL:    "http://example.com/cf-speedtest-linux-amd64.tar.gz",
				Size:   9486049,
				SHA256: "abc123def456",
			},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)

	// 1. 启动 version.json 服务器
	versionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(manifestJSON)
	}))
	defer versionServer.Close()

	// 2. 启动代理服务器
	proxy := newTestProxy(t)
	defer proxy.Close()

	// 3. 创建带代理的 Checker
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	checker := NewChecker(versionServer.URL+"/version.json", "1.0.0", proxy.URL(), logger)
	if checker == nil {
		t.Fatal("NewChecker 返回 nil")
	}

	// 4. 执行版本检查
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, hasUpdate, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check 通过代理失败: %v", err)
	}

	// 5. 验证代理被使用
	if proxy.RequestCount() == 0 {
		t.Error("代理未被使用:请求数为 0,说明 Checker 没有通过代理请求")
	}

	// 6. 验证 manifest 内容
	if result == nil {
		t.Fatal("Check 返回 nil manifest")
	}
	if result.Version != "1.2.0" {
		t.Errorf("版本号不匹配: 期望 1.2.0, 实际 %s", result.Version)
	}
	if !hasUpdate {
		t.Error("应检测到有更新(当前 1.0.0 → 最新 1.2.0)")
	}
	if result.MinRequiredVersion != "1.0.0" {
		t.Errorf("最低兼容版本不匹配: 期望 1.0.0, 实际 %s", result.MinRequiredVersion)
	}

	// 7. 验证 assets 信息
	asset, ok := result.Assets["linux/amd64"]
	if !ok {
		t.Error("未找到 linux/amd64 的 asset")
	}
	if asset.Size != 9486049 {
		t.Errorf("asset size 不匹配: 期望 9486049, 实际 %d", asset.Size)
	}

	t.Logf("✓ 代理版本检查成功: 最新版本 %s, hasUpdate=%v, 代理请求数: %d",
		result.Version, hasUpdate, proxy.RequestCount())
}

// TestProxyDownloadLargeFile 验证代理下载大文件(支持断点续传场景)
func TestProxyDownloadLargeFile(t *testing.T) {
	// 准备较大的测试文件(约 1MB,模拟真实更新包)
	fileContent := bytes.Repeat([]byte("LARGE-FILE-CHUNK-"), 60000) // ~1.08MB

	// 1. 启动文件服务器
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 支持 Range 请求(断点续传)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	// 2. 启动代理
	proxy := newTestProxy(t)
	defer proxy.Close()

	// 3. 创建 Downloader(带代理)
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	dl := NewDownloader(t.TempDir(), proxy.URL(), logger)

	// 4. 下载
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lastProgress int64
	downloadPath, err := dl.Download(ctx, fileServer.URL+"/large-file.tar.gz", "large-file.tar.gz",
		int64(len(fileContent)),
		func(downloaded, total int64, speed float64, eta time.Duration) {
			if downloaded > lastProgress {
				lastProgress = downloaded
			}
		},
	)
	if err != nil {
		t.Fatalf("大文件代理下载失败: %v", err)
	}

	// 5. 验证
	if proxy.RequestCount() == 0 {
		t.Error("代理未被使用")
	}

	downloaded, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatalf("读取下载文件失败: %v", err)
	}
	if len(downloaded) != len(fileContent) {
		t.Errorf("文件大小不匹配: 期望 %d, 实际 %d", len(fileContent), len(downloaded))
	}
	if !bytes.Equal(downloaded, fileContent) {
		t.Error("下载内容不匹配")
	}

	t.Logf("✓ 大文件代理下载成功: %d 字节 (%.1f MB), 代理请求数: %d",
		len(downloaded), float64(len(downloaded))/1024/1024, proxy.RequestCount())
}

// TestNoProxyDirectDownload 对照组:不配置代理时直接下载
func TestNoProxyDirectDownload(t *testing.T) {
	fileContent := []byte("direct-download-test-content")

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	// 不配置代理(proxy="")
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	dl := NewDownloader(t.TempDir(), "", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	downloadPath, err := dl.Download(ctx, fileServer.URL+"/test.txt", "test.txt", int64(len(fileContent)),
		func(d, t int64, s float64, eta time.Duration) {},
	)
	if err != nil {
		t.Fatalf("直连下载失败: %v", err)
	}

	downloaded, _ := os.ReadFile(downloadPath)
	if !bytes.Equal(downloaded, fileContent) {
		t.Error("直连下载内容不匹配")
	}

	t.Logf("✓ 直连下载成功: %d 字节", len(downloaded))
}
