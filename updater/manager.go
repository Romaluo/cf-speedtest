package updater

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"cf-speedtest/log"
)

// Restarter 由调用方(web.Server)实现,用于触发进程重启
// Phase 3 中 Manager 在安装完成后调用此接口
type Restarter interface {
	Restart() error
}

// Manager 更新管理器(状态机 + SSE 进度推送)
// 协调 Checker、Downloader、Installer 完成一键更新流程
//
// 状态机:idle → checking → downloading → verifying → extracting → installing → restarting → done
// 失败任意状态 → failed(终态,等待用户重新触发)
type Manager struct {
	checker    *Checker
	downloader *Downloader
	installer  *Installer
	logger     *log.Logger
	restarter  Restarter // 可为 nil(Phase 3 注入)

	mu           sync.Mutex
	currentState State
	cancelFunc   context.CancelFunc // 用于取消进行中的下载

	subMu       sync.RWMutex
	subscribers map[chan ProgressEvent]struct{}
}

// NewManager 创建更新管理器
func NewManager(checker *Checker, downloader *Downloader, logger *log.Logger) *Manager {
	return &Manager{
		checker:      checker,
		downloader:   downloader,
		installer:    NewInstaller(logger),
		logger:       logger,
		currentState: StateIdle,
		subscribers:  make(map[chan ProgressEvent]struct{}),
	}
}

// SetRestarter 注入重启器(Phase 3 调用)
func (m *Manager) SetRestarter(r Restarter) {
	m.mu.Lock()
	m.restarter = r
	m.mu.Unlock()
}

// getRestarter 线程安全地读取 Restarter(避免与 SetRestarter 数据竞争)
func (m *Manager) getRestarter() Restarter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restarter
}

// State 返回当前状态(线程安全)
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentState
}

// Apply 触发一键更新流程(异步执行)
// 立即返回,后台 goroutine 执行:checking → downloading → verifying → extracting → installing → restarting
// 如果当前 state != idle && != failed,返回 ErrUpdateInProgress
// 如果 checker/downloader 未初始化,返回相应错误
func (m *Manager) Apply() error {
	// 参数校验放在锁外,避免在持有锁时返回错误
	if m.checker == nil {
		return ErrCheckerNotInitialized
	}
	if m.downloader == nil {
		return ErrDownloaderNotInitialized
	}

	m.mu.Lock()
	if m.currentState != StateIdle && m.currentState != StateFailed {
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Warn("UPDATE", "Apply 被拒绝: 当前状态 %s (更新已在进行中)", m.currentState)
		}
		return ErrUpdateInProgress
	}
	prev := m.currentState
	// 设置 state=checking,防止并发触发
	m.currentState = StateChecking
	// 启动新的 ctx 用于取消下载
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	currentVersion := m.checker.CurrentVersion()
	m.mu.Unlock()

	if m.logger != nil {
		m.logger.Info("UPDATE", "Apply 触发更新流程: %s → checking (当前版本 %s)",
			prev, currentVersion)
	}
	go m.runUpdate(ctx)
	return nil
}

// Check 执行同步版本检查(不影响更新流程,仅刷新 manifest)
// 短暂设置 state=checking,完成后回到 idle(无论成功失败,因为这只是检查)
// 如果当前有更新流程在进行(state != idle && != failed),返回 ErrUpdateInProgress
func (m *Manager) Check(ctx context.Context) (*VersionManifest, bool, error) {
	m.mu.Lock()
	if m.currentState != StateIdle && m.currentState != StateFailed {
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Warn("UPDATE", "Check 被拒绝: 当前状态 %s", m.currentState)
		}
		return nil, false, ErrUpdateInProgress
	}
	m.currentState = StateChecking
	m.mu.Unlock()

	if m.logger != nil {
		m.logger.Info("UPDATE", "Check 执行手动版本检查")
	}

	// 完成后回到 idle(无论成功失败,因为这只是检查,不是更新流程)
	defer func() {
		m.mu.Lock()
		// 只在仍是 checking 时回 idle(避免覆盖 runUpdate 设置的其他状态)
		if m.currentState == StateChecking {
			m.currentState = StateIdle
		}
		m.mu.Unlock()
	}()

	return m.checker.Check(ctx)
}

// Cancel 取消进行中的更新(仅 downloading/verifying 状态有效)
// 已完成或已失败的流程调用此方法无效果
func (m *Manager) Cancel() {
	m.mu.Lock()
	state := m.currentState
	cancelFn := m.cancelFunc
	// 用后立即清空,避免下次 Apply 复用已执行过的 cancel(防止误取消新流程)
	m.cancelFunc = nil
	m.mu.Unlock()

	if cancelFn == nil {
		if m.logger != nil {
			m.logger.Warn("UPDATE", "Cancel 无效: 无进行中的更新流程 (state=%s)", state)
		}
		return
	}
	if state != StateDownloading && state != StateVerifying {
		if m.logger != nil {
			m.logger.Warn("UPDATE", "Cancel 无效: 当前状态 %s 不支持取消(仅 downloading/verifying 可取消)", state)
		}
		return
	}
	if m.logger != nil {
		m.logger.Info("UPDATE", "Cancel 取消更新流程 (state=%s)", state)
	}
	cancelFn()
}

// Subscribe 订阅进度事件
// 返回 channel(缓冲 32),调用方应通过 Unsubscribe 释放
// 事件类型:StateDownloading(进度)、StateVerifying、StateInstalling 等
func (m *Manager) Subscribe() chan ProgressEvent {
	ch := make(chan ProgressEvent, 32)
	m.subMu.Lock()
	m.subscribers[ch] = struct{}{}
	count := len(m.subscribers)
	m.subMu.Unlock()
	if m.logger != nil {
		m.logger.Debug("UPDATE", "新增 SSE 订阅者 (当前总数: %d)", count)
	}
	return ch
}

// Unsubscribe 取消订阅并关闭 channel
func (m *Manager) Unsubscribe(ch chan ProgressEvent) {
	m.subMu.Lock()
	delete(m.subscribers, ch)
	count := len(m.subscribers)
	m.subMu.Unlock()
	close(ch)
	if m.logger != nil {
		m.logger.Debug("UPDATE", "移除 SSE 订阅者 (当前总数: %d)", count)
	}
}

// setState 设置状态并推送事件给所有订阅者
func (m *Manager) setState(s State, msg string) {
	m.mu.Lock()
	prev := m.currentState
	m.currentState = s
	m.mu.Unlock()
	if m.logger != nil {
		m.logger.Info("UPDATE", "状态转换: %s → %s (%s)", prev, s, msg)
	}
	m.broadcast(ProgressEvent{
		State:   s,
		Message: msg,
	})
}

// setError 设置 failed 状态并推送错误事件
func (m *Manager) setError(err error) {
	m.mu.Lock()
	prev := m.currentState
	m.currentState = StateFailed
	m.mu.Unlock()
	if m.logger != nil {
		m.logger.Error("UPDATE", "更新失败: %s → failed: %v", prev, err)
	}
	m.broadcast(ProgressEvent{
		State: StateFailed,
		Error: err.Error(),
	})
}

// broadcast 推送事件给所有订阅者(非阻塞,缓冲满则丢弃避免阻塞主流程)
func (m *Manager) broadcast(ev ProgressEvent) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()
	for ch := range m.subscribers {
		select {
		case ch <- ev:
		default:
			// 缓冲满,丢弃
		}
	}
}

// runUpdate 实际的更新流程(在 goroutine 中执行)
// 流程: checking → downloading → verifying → extracting → installing → restarting
// 任一阶段失败 → failed 状态(等待用户重新触发)
func (m *Manager) runUpdate(ctx context.Context) {
	updateStart := time.Now()
	if m.logger != nil {
		m.logger.Info("UPDATE", "===== 开始一键更新流程 =====")
	}
	defer func() {
		// 异常恢复:panic 时设置 failed,避免 goroutine 静默退出
		if r := recover(); r != nil {
			if m.logger != nil {
				m.logger.Error("UPDATE", "更新流程 panic: %v", r)
			}
			m.setError(fmt.Errorf("内部错误: %v", r))
		}
	}()

	// Step 1: checking(复用 Checker 已缓存的 manifest,如果没有则重新拉取)
	// 注:Apply 中已做 nil 检查,此处双保险防御并发场景下组件被意外置空
	if m.checker == nil {
		m.setError(ErrCheckerNotInitialized)
		return
	}
	if m.downloader == nil {
		m.setError(ErrDownloaderNotInitialized)
		return
	}
	if m.installer == nil {
		m.setError(ErrInstallerNotInitialized)
		return
	}
	manifest := m.checker.Manifest()
	if manifest != nil {
		if m.logger != nil {
			m.logger.Info("UPDATE", "使用缓存的 manifest (v%s, 跳过版本检查)", manifest.Version)
		}
	} else {
		m.setState(StateChecking, "正在检查版本...")
		var hasUpdate bool
		var err error
		manifest, hasUpdate, err = m.checker.Check(ctx)
		if err != nil {
			m.setError(fmt.Errorf("版本检查失败: %w", err))
			return
		}
		if !hasUpdate {
			if m.logger != nil {
				m.logger.Info("UPDATE", "已是最新版本 v%s,无需更新", manifest.Version)
			}
			m.setState(StateIdle, "已是最新版本,无需更新")
			return
		}
	}

	// Step 2: downloading
	m.setState(StateDownloading, fmt.Sprintf("正在下载 v%s...", manifest.Version))
	asset, err := m.checker.CurrentAsset()
	if err != nil {
		m.setError(err)
		return
	}
	if m.logger != nil {
		m.logger.Info("UPDATE", "下载资产信息: %s/%s, size=%d, sha256=%s",
			runtime.GOOS, runtime.GOARCH, asset.Size, truncateSHA(asset.SHA256))
	}
	// 临时文件名:cf-speedtest-update-{version}-{goos}-{goarch}.{ext}
	filename := fmt.Sprintf("cf-speedtest-update-%s-%s-%s", manifest.Version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		filename += ".zip"
	} else {
		filename += ".tar.gz"
	}

	downloadPath, err := m.downloader.Download(ctx, asset.URL, filename, asset.Size, func(downloaded, total int64, speed float64, eta time.Duration) {
		var percent float64
		if total > 0 {
			percent = float64(downloaded) / float64(total) * 100
		}
		m.broadcast(ProgressEvent{
			State:   StateDownloading,
			Percent: percent,
			Speed:   formatSpeed(speed),
			ETA:     formatDuration(eta),
		})
	})
	if err != nil {
		if ctx.Err() != nil {
			// 用户取消
			m.setState(StateIdle, "下载已取消")
		} else {
			m.setError(fmt.Errorf("下载失败: %w", err))
		}
		return
	}

	// Step 3: verifying
	m.setState(StateVerifying, "正在校验 sha256...")
	result, err := m.downloader.Verify(downloadPath, asset.Size, asset.SHA256)
	if err != nil {
		m.downloader.Cleanup(downloadPath)
		m.setError(fmt.Errorf("校验失败: %w", err))
		return
	}
	if m.logger != nil {
		m.logger.Info("UPDATE", "下载校验成功: %s (sha256=%s, size=%d)",
			result.Path, result.SHA256, result.Size)
	}

	// Step 4: extracting(解压更新包)
	m.setState(StateExtracting, "正在解压更新包...")
	extractDir, err := m.installer.Extract(downloadPath, manifest.Version)
	if err != nil {
		// 解压失败:清理下载文件,避免残留
		m.downloader.Cleanup(downloadPath)
		m.setError(fmt.Errorf("解压失败: %w", err))
		return
	}

	// Step 5: installing(替换二进制 + 资源文件)
	m.setState(StateInstalling, "正在替换二进制文件...")
	if err := m.installer.Install(extractDir); err != nil {
		// 安装失败:清理临时文件 + 回滚(Installer 内部已尝试回滚,这里只清理)
		m.installer.Cleanup(extractDir)
		m.downloader.Cleanup(downloadPath)
		m.setError(fmt.Errorf("安装失败: %w", err))
		return
	}

	// 安装成功:清理临时解压目录 + 下载文件(避免占用磁盘)
	m.installer.Cleanup(extractDir)
	m.downloader.Cleanup(downloadPath)

	// Step 6: restarting(触发进程重启)
	m.setState(StateRestarting, "更新完成,正在重启服务...")

	// 推送 done 事件,告知前端即将重启(前端可显示"重启中"提示)
	m.broadcast(ProgressEvent{
		State:   StateDone,
		Message: fmt.Sprintf("已更新到 v%s,服务即将重启", manifest.Version),
	})

	if m.logger != nil {
		m.logger.Info("UPDATE", "更新成功,版本 %s → %s,触发重启 (总耗时 %s)",
			m.checker.CurrentVersion(), manifest.Version,
			time.Since(updateStart).Round(time.Millisecond))
	}

	// 调用注入的 Restarter 重启进程(web.Server 实现)
	// Restarter 内部会:Linux systemd 触发 systemctl restart;非 systemd fork-exec
	// 如果未注入 Restarter,仅记录警告(用户需手动重启)
	restarter := m.getRestarter()
	if restarter != nil {
		// 异步触发重启,避免阻塞 broadcast 给所有订阅者
		go func() {
			defer func() {
				// panic 恢复:Restart 内部异常不应导致 goroutine 静默退出
				if r := recover(); r != nil {
					if m.logger != nil {
						m.logger.Error("UPDATE", "Restart panic: %v(更新已完成,请手动重启)", r)
					}
					m.setState(StateIdle, "更新已完成,请手动重启服务")
				}
			}()
			if err := restarter.Restart(); err != nil {
				if m.logger != nil {
					m.logger.Error("UPDATE", "重启失败: %v(更新已完成,请手动重启)", err)
				}
				// 重启失败:回到 idle 状态,允许用户通过 /api/system/restart 手动重启
				m.setState(StateIdle, "更新已完成,请手动重启服务")
			}
		}()
	} else {
		if m.logger != nil {
			m.logger.Warn("UPDATE", "未注入 Restarter,更新已完成但需手动重启")
		}
		m.setState(StateIdle, "更新已完成,请手动重启服务")
	}
}

// formatSpeed 格式化下载速度
func formatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return ""
	}
	switch {
	case bytesPerSec >= 1024*1024:
		return fmt.Sprintf("%.2f MB/s", bytesPerSec/1024/1024)
	case bytesPerSec >= 1024:
		return fmt.Sprintf("%.2f KB/s", bytesPerSec/1024)
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

// formatDuration 格式化持续时间(ETA)
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)
	}
	return fmt.Sprintf("%02d:%02d", mins, secs)
}

// ErrUpdateInProgress 当前已有更新在进行中
var ErrUpdateInProgress = errors.New("更新流程已在进行中")

// ErrCheckerNotInitialized Checker 未初始化
var ErrCheckerNotInitialized = errors.New("版本检查器未初始化")

// ErrDownloaderNotInitialized Downloader 未初始化
var ErrDownloaderNotInitialized = errors.New("下载器未初始化")

// ErrInstallerNotInitialized Installer 未初始化
var ErrInstallerNotInitialized = errors.New("安装器未初始化")
