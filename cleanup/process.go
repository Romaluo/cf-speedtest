package cleanup

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ProcessTracker 子进程跟踪器
// 跟踪任务执行期间启动的子进程，确保任务完成后终止残留进程
type ProcessTracker struct {
	mu       sync.Mutex
	processes map[int]*exec.Cmd // PID -> Cmd
	timeout  time.Duration
}

// NewProcessTracker 创建进程跟踪器
func NewProcessTracker(timeout time.Duration) *ProcessTracker {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ProcessTracker{
		processes: make(map[int]*exec.Cmd),
		timeout:   timeout,
	}
}

// Register 注册一个子进程用于跟踪
// 在 cmd.Start() 之后调用
func (t *ProcessTracker) Register(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.processes[cmd.Process.Pid] = cmd
}

// Unregister 注销一个子进程（进程正常退出后调用）
func (t *ProcessTracker) Unregister(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.processes, pid)
}

// KillAll 终止所有被跟踪的子进程
// 返回成功终止的进程数和错误列表
func (t *ProcessTracker) KillAll() (int, []error) {
	t.mu.Lock()
	pids := make([]int, 0, len(t.processes))
	for pid := range t.processes {
		pids = append(pids, pid)
	}
	t.mu.Unlock()

	if len(pids) == 0 {
		return 0, nil
	}

	killed := 0
	var errs []error

	for _, pid := range pids {
		// 检查进程是否仍在运行
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}

		// 发送 SIGTERM 优雅终止
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			// 进程可能已退出
			if err.Error() == "os: process already finished" {
				t.Unregister(pid)
				continue
			}
			errs = append(errs, fmt.Errorf("SIGTERM 进程 %d 失败: %w", pid, err))
			continue
		}

		// 等待进程退出（带超时）
		if !waitForExit(pid, t.timeout) {
			// 超时后强制 SIGKILL
			if err := proc.Signal(syscall.SIGKILL); err != nil {
				errs = append(errs, fmt.Errorf("SIGKILL 进程 %d 失败: %w", pid, err))
				continue
			}
			_, _ = proc.Wait()
		}
		killed++
		t.Unregister(pid)
	}

	return killed, errs
}

// waitForExit 等待进程退出，最多等待 timeout
// 返回 true 表示进程已退出，false 表示超时
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		// 发送 signal 0 检测进程是否存在
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// Count 返回当前跟踪的进程数
func (t *ProcessTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.processes)
}
