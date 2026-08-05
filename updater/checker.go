package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"cf-speedtest/log"
)

// Checker 版本检查器
// 职责:HTTP GET version.json → 解析 → 对比当前版本 → 缓存到内存
// 不负责下载/安装,这些由 manager + downloader + installer 协作完成
type Checker struct {
	url            string        // version.json 的 URL
	currentVersion string        // 当前运行的版本号
	httpClient     *http.Client  // HTTP 客户端(带超时)
	logger         *log.Logger   // 日志记录器(可为 nil)

	mu         sync.RWMutex
	manifest   *VersionManifest // 最近一次成功拉取到的 manifest(可能为 nil)
	lastCheckAt time.Time       // 最近一次检查时间(无论成功失败)
	lastError  string           // 最近一次检查错误(空=无错误)
}

// NewChecker 创建版本检查器
// url: version.json 的完整 URL(通常为 raw.githubusercontent.com)
// currentVersion: 当前进程版本号(对应 main.go 的 version 常量)
// url 为空或 currentVersion 为空时返回 nil(调用方需检查)
func NewChecker(url, currentVersion string, logger *log.Logger) *Checker {
	if url == "" || currentVersion == "" {
		if logger != nil {
			logger.Warn("UPDATE", "NewChecker 参数无效: url=%q currentVersion=%q (返回 nil)", url, currentVersion)
		}
		return nil
	}
	return &Checker{
		url:            url,
		currentVersion: currentVersion,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		logger:         logger,
	}
}

// Check 执行一次版本检查
// 流程:HTTP GET version.json → 解析 → 校验最低兼容版本 → 缓存 manifest
// 返回值:
//   - manifest: 最新拉取到的 manifest(检查失败时为 nil)
//   - hasUpdate: manifest.Version 是否大于 currentVersion
//   - err: 检查过程中的错误(网络/解析/降级保护)
func (c *Checker) Check(ctx context.Context) (*VersionManifest, bool, error) {
	c.mu.Lock()
	c.lastCheckAt = time.Now()
	c.mu.Unlock()

	if c.logger != nil {
		c.logger.Info("UPDATE", "开始检查版本更新: %s", c.url)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		c.setLastError(err.Error())
		return nil, false, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// 禁用缓存,确保拿到最新 version.json(通过 If-None-Match + Cache-Control)
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.setLastError(err.Error())
		return nil, false, fmt.Errorf("拉取 version.json 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("version.json 返回非 200: HTTP %d", resp.StatusCode)
		c.setLastError(err.Error())
		return nil, false, err
	}

	var m VersionManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		c.setLastError(fmt.Sprintf("解析 version.json 失败: %v", err))
		return nil, false, fmt.Errorf("解析 version.json 失败: %w", err)
	}

	// 基础字段校验
	if m.Version == "" {
		err := fmt.Errorf("version.json 缺少 version 字段")
		c.setLastError(err.Error())
		return nil, false, err
	}
	if len(m.Assets) == 0 {
		c.setLastError("version.json 缺少 assets 字段")
		// 仍然缓存 manifest,前端可展示版本信息但不能下载
	}

	// 降级保护: 当前版本低于最低兼容版本 → 拒绝更新(避免不兼容)
	if m.MinRequiredVersion != "" && compareVersions(c.currentVersion, m.MinRequiredVersion) < 0 {
		err := fmt.Errorf("当前版本 %s 低于最低兼容版本 %s,请先升级到中间版本", c.currentVersion, m.MinRequiredVersion)
		c.setLastError(err.Error())
		return nil, false, err
	}

	hasUpdate := compareVersions(m.Version, c.currentVersion) > 0

	c.mu.Lock()
	c.manifest = &m
	c.lastError = ""
	c.mu.Unlock()

	if c.logger != nil {
		if hasUpdate {
			c.logger.Info("UPDATE", "发现新版本: 当前 %s → 最新 %s", c.currentVersion, m.Version)
		} else {
			c.logger.Info("UPDATE", "已是最新版本: %s", c.currentVersion)
		}
	}
	return &m, hasUpdate, nil
}

// CurrentAsset 返回当前平台(GOOS/GOARCH)的下载资产信息
// 在没有 manifest 或当前平台未提供资产时返回错误
func (c *Checker) CurrentAsset() (*PlatformAsset, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.manifest == nil {
		return nil, fmt.Errorf("尚未拉取 version.json,无 manifest")
	}
	key := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	asset, ok := c.manifest.Assets[key]
	if !ok {
		return nil, fmt.Errorf("version.json 未提供当前平台 %s 的下载资产", key)
	}
	return &asset, nil
}

// Status 构造 StatusResponse
// currentState: 由 Manager 持有的当前更新流程状态(检查器自身不维护更新流程状态)
func (c *Checker) Status(currentState State) StatusResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resp := StatusResponse{
		CurrentVersion: c.currentVersion,
		LastCheckAt:    c.lastCheckAt,
		LastCheckError: c.lastError,
		State:          currentState,
	}
	if c.manifest != nil {
		resp.LatestVersion = c.manifest.Version
		resp.ReleaseNotes = c.manifest.ReleaseNotes
		resp.PublishedAt = c.manifest.PublishedAt
		resp.MinRequiredVersion = c.manifest.MinRequiredVersion
		// 直接读 manifest.Assets,避免与外层 RLock 死锁
		key := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
		if asset, ok := c.manifest.Assets[key]; ok {
			resp.UpdateSize = asset.Size
		}
		resp.HasUpdate = compareVersions(c.manifest.Version, c.currentVersion) > 0
	} else {
		// 还未成功检查过,latest 等于 current,hasUpdate=false
		resp.LatestVersion = c.currentVersion
	}
	return resp
}

// Manifest 返回最近一次成功拉取的 manifest(可能为 nil)
// 调用方不应修改返回的指针(只读)
func (c *Checker) Manifest() *VersionManifest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.manifest
}

// CurrentVersion 返回当前版本号
func (c *Checker) CurrentVersion() string {
	return c.currentVersion
}

// URL 返回 version.json 的 URL
func (c *Checker) URL() string {
	return c.url
}

// setLastError 设置最近一次检查的错误信息(线程安全)
func (c *Checker) setLastError(msg string) {
	c.mu.Lock()
	c.lastError = msg
	c.mu.Unlock()
}

// compareVersions 语义化版本比较
// 返回: >0 表示 a>b, <0 表示 a<b, 0 表示相等
// 仅比较主版本号(major.minor.patch),预发布标识(-beta 等)被忽略
func compareVersions(a, b string) int {
	aParts := parseVersion(a)
	bParts := parseVersion(b)
	max := len(aParts)
	if len(bParts) > max {
		max = len(bParts)
	}
	for i := 0; i < max; i++ {
		var av, bv int
		if i < len(aParts) {
			av = aParts[i]
		}
		if i < len(bParts) {
			bv = bParts[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}

// parseVersion 解析版本号字符串为整数切片
// 支持 "1.2.3" / "v1.2.3" / "V1.2.3" / "1.2.3-beta" / "1.2.3+build5" 等格式
// 非数字部分会终止当前段的解析
func parseVersion(s string) []int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	// 截断预发布(-alpha)和构建元数据(+build)
	if idx := strings.IndexAny(s, "-+"); idx > 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		result = append(result, n)
	}
	return result
}
