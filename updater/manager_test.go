package updater

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cf-speedtest/log"
)

// fakeRestarter 用于测试 Manager 重启流程
type fakeRestarter struct {
	mu      sync.Mutex
	called  bool
	errFunc func() error // 返回 nil 表示重启成功
}

func (f *fakeRestarter) Restart() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	if f.errFunc != nil {
		return f.errFunc()
	}
	return nil
}

func (f *fakeRestarter) wasCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

// newTestManager 创建用于测试的 Manager(带 mock checker/downloader)
func newTestManager(t *testing.T) (*Manager, *fakeRestarter) {
	t.Helper()
	logger, err := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	if err != nil {
		t.Fatalf("创建 logger 失败: %v", err)
	}
	checker := NewChecker("http://localhost/version.json", "1.0.0", "", logger)
	if checker == nil {
		t.Fatalf("NewChecker 返回 nil")
	}
	dl := NewDownloader(t.TempDir(), "", logger)
	mgr := NewManager(checker, dl, logger)
	if mgr == nil {
		t.Fatalf("NewManager 返回 nil")
	}
	r := &fakeRestarter{}
	mgr.SetRestarter(r)
	return mgr, r
}

// TestApply_RejectConcurrentTrigger 并发 Apply 应被拒绝
func TestApply_RejectConcurrentTrigger(t *testing.T) {
	mgr, _ := newTestManager(t)

	// 第一次 Apply 应成功(state 从 idle 转为 checking)
	if err := mgr.Apply(); err != nil {
		t.Fatalf("第一次 Apply 应成功, got: %v", err)
	}

	// 第二次 Apply 应返回 ErrUpdateInProgress(状态非 idle/failed)
	err := mgr.Apply()
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Errorf("第二次 Apply 应返回 ErrUpdateInProgress, got: %v", err)
	}

	// 等待 goroutine 退出(它会很快失败,因为 URL 不可达)
	// 手动设置 state=idle 以便清理
	mgr.mu.Lock()
	mgr.currentState = StateIdle
	mgr.mu.Unlock()
}

// TestApply_CheckerNil checker 为 nil 时应返回 ErrCheckerNotInitialized
func TestApply_CheckerNil(t *testing.T) {
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	mgr := &Manager{
		checker:      nil,
		downloader:   NewDownloader(t.TempDir(), "", logger),
		installer:    NewInstaller(logger),
		logger:       logger,
		currentState: StateIdle,
		subscribers:  make(map[chan ProgressEvent]struct{}),
	}
	err := mgr.Apply()
	if !errors.Is(err, ErrCheckerNotInitialized) {
		t.Errorf("checker 为 nil 时应返回 ErrCheckerNotInitialized, got: %v", err)
	}
}

// TestApply_DownloaderNil downloader 为 nil 时应返回 ErrDownloaderNotInitialized
func TestApply_DownloaderNil(t *testing.T) {
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	checker := NewChecker("http://localhost/version.json", "1.0.0", "", logger)
	mgr := &Manager{
		checker:      checker,
		downloader:   nil,
		installer:    NewInstaller(logger),
		logger:       logger,
		currentState: StateIdle,
		subscribers:  make(map[chan ProgressEvent]struct{}),
	}
	err := mgr.Apply()
	if !errors.Is(err, ErrDownloaderNotInitialized) {
		t.Errorf("downloader 为 nil 时应返回 ErrDownloaderNotInitialized, got: %v", err)
	}
}

// TestCancel_NoActiveUpdate 无进行中的更新时 Cancel 应安全返回
func TestCancel_NoActiveUpdate(t *testing.T) {
	mgr, _ := newTestManager(t)
	// 此时 state=idle,cancelFunc=nil,Cancel 应安全返回(nil)
	mgr.Cancel()
}

// TestCancel_AfterApply Apply 后 Cancel 应能调用 cancelFunc
func TestCancel_AfterApply(t *testing.T) {
	mgr, _ := newTestManager(t)
	// Apply 触发后台 goroutine(会很快失败因为 URL 不可达)
	_ = mgr.Apply()
	// 给一点时间让 goroutine 启动
	time.Sleep(10 * time.Millisecond)
	// Cancel 应能安全调用,即使 goroutine 可能已完成
	mgr.Cancel()
}

// TestSubscribeUnsubscribe 订阅/取消订阅
func TestSubscribeUnsubscribe(t *testing.T) {
	mgr, _ := newTestManager(t)
	ch := mgr.Subscribe()
	if ch == nil {
		t.Fatalf("Subscribe 返回 nil channel")
	}
	// 验证订阅已加入 map
	mgr.subMu.RLock()
	count := len(mgr.subscribers)
	mgr.subMu.RUnlock()
	if count != 1 {
		t.Errorf("订阅后订阅者数应为 1, got %d", count)
	}
	mgr.Unsubscribe(ch)
	mgr.subMu.RLock()
	count = len(mgr.subscribers)
	mgr.subMu.RUnlock()
	if count != 0 {
		t.Errorf("取消订阅后订阅者数应为 0, got %d", count)
	}
}

// TestBroadcast_BufferFull 广播缓冲满应丢弃而不阻塞
func TestBroadcast_BufferFull(t *testing.T) {
	mgr, _ := newTestManager(t)
	ch := mgr.Subscribe()
	// 不消费 ch,填充到缓冲满(32),然后继续广播应不阻塞
	for i := 0; i < 100; i++ {
		mgr.broadcast(ProgressEvent{
			State:   StateDownloading,
			Percent: float64(i),
		})
	}
	// 能到达这里说明 broadcast 未阻塞
	select {
	case <-ch:
		// 有事件可读
	default:
		t.Errorf("应有事件可读")
	}
}

// TestCheck_RejectConcurrentCheck 并发 Check 应被拒绝
func TestCheck_RejectConcurrentCheck(t *testing.T) {
	mgr, _ := newTestManager(t)
	// 将 state 设为 downloading 模拟更新进行中
	mgr.mu.Lock()
	mgr.currentState = StateDownloading
	mgr.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, _, err := mgr.Check(ctx)
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Errorf("更新进行中 Check 应返回 ErrUpdateInProgress, got: %v", err)
	}
}

// TestNewChecker_EmptyParams 空参数应返回 nil
func TestNewChecker_EmptyParams(t *testing.T) {
	logger, _ := log.NewLogger(filepath.Join(t.TempDir(), "test.log"), log.LevelDebug)
	if c := NewChecker("", "1.0.0", "", logger); c != nil {
		t.Errorf("空 url 时 NewChecker 应返回 nil")
	}
	if c := NewChecker("http://x", "", "", logger); c != nil {
		t.Errorf("空 currentVersion 时 NewChecker 应返回 nil")
	}
}

// TestManager_State 状态机基本转换
func TestManager_State(t *testing.T) {
	mgr, _ := newTestManager(t)
	if got := mgr.State(); got != StateIdle {
		t.Errorf("初始状态应为 StateIdle, got %v", got)
	}
	mgr.setState(StateChecking, "test")
	if got := mgr.State(); got != StateChecking {
		t.Errorf("setState 后状态应为 StateChecking, got %v", got)
	}
}

// TestManager_getRestarter 并发安全读取 restarter
func TestManager_getRestarter(t *testing.T) {
	mgr, r := newTestManager(t)
	if got := mgr.getRestarter(); got == nil {
		t.Errorf("getRestarter 应返回注入的 Restarter, got nil")
	}
	// 并发读取 + 设置,确保不 panic
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = mgr.getRestarter()
		}()
		go func() {
			defer wg.Done()
			mgr.SetRestarter(r)
		}()
	}
	wg.Wait()
}

// TestTruncateSHA SHA 截断
func TestTruncateSHA(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"abc", "abc"},
		{"abcdef", "abcdef"},
		{"abcdefg", "abcdefg"},
		{"abcdefghijklmn", "abcdefghijkl..."}, // 长度 > 12 时截断
		{"", ""},
	}
	for _, tc := range cases {
		got := truncateSHA(tc.input)
		if got != tc.want {
			t.Errorf("truncateSHA(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
