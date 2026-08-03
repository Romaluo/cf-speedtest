package web

import (
	"cf-speedtest/config"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DaemonControl 后台模式运行时控制器
// 管理 daemon 模式的激活状态和定时任务调度信息
type DaemonControl struct {
	mu          sync.RWMutex
	active      bool      // 定时任务是否激活
	nextCollect time.Time // 下次采集时间
	nextPush    time.Time // 下次推送时间
	toggleCh    chan bool // 切换通知通道（true=激活, false=停止）
}

// NewDaemonControl 创建控制器
func NewDaemonControl(active bool) *DaemonControl {
	return &DaemonControl{
		active:   active,
		toggleCh: make(chan bool, 1),
	}
}

// IsActive 返回定时任务是否激活
func (dc *DaemonControl) IsActive() bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.active
}

// SetActive 设置激活状态（不通知通道，仅内部使用）
func (dc *DaemonControl) SetActive(v bool) {
	dc.mu.Lock()
	dc.active = v
	dc.mu.Unlock()
}

// SetNextCollect 更新下次采集时间
func (dc *DaemonControl) SetNextCollect(t time.Time) {
	dc.mu.Lock()
	dc.nextCollect = t
	dc.mu.Unlock()
}

// SetNextPush 更新下次推送时间
func (dc *DaemonControl) SetNextPush(t time.Time) {
	dc.mu.Lock()
	dc.nextPush = t
	dc.mu.Unlock()
}

// Status 返回当前状态信息
func (dc *DaemonControl) Status() map[string]interface{} {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	nc := ""
	if !dc.nextCollect.IsZero() {
		nc = dc.nextCollect.Format("2006-01-02 15:04:05")
	}
	np := ""
	if !dc.nextPush.IsZero() {
		np = dc.nextPush.Format("2006-01-02 15:04:05")
	}
	return map[string]interface{}{
		"active":       dc.active,
		"next_collect": nc,
		"next_push":    np,
	}
}

// ToggleChan 返回切换通知通道
func (dc *DaemonControl) ToggleChan() <-chan bool {
	return dc.toggleCh
}

// Toggle 切换激活状态并通知 runDaemon
func (dc *DaemonControl) Toggle() bool {
	dc.mu.Lock()
	dc.active = !dc.active
	newState := dc.active
	dc.mu.Unlock()
	// 非阻塞发送切换信号
	select {
	case dc.toggleCh <- newState:
	default:
		// 通道满时清空再发
		select {
		case <-dc.toggleCh:
		default:
		}
		dc.toggleCh <- newState
	}
	return newState
}

// daemonStatusHandler GET /api/daemon/status
func (srv *Server) daemonStatusHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Daemon == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"active": false, "next_collect": "", "next_push": "", "job_running": false})
		return
	}
	status := srv.deps.Daemon.Status()
	// 附带当前是否有测速/推送任务在运行,供前端锁定定时任务按钮
	status["job_running"] = srv.jobs.current() != nil
	json.NewEncoder(w).Encode(status)
}

// daemonToggleHandler POST /api/daemon/toggle
func (srv *Server) daemonToggleHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Daemon == nil {
		http.Error(w, `{"error":"daemon controller not available"}`, http.StatusInternalServerError)
		return
	}
	// 测速/推送任务运行中时禁止切换定时任务,避免状态冲突
	if j := srv.jobs.current(); j != nil {
		srv.deps.Logger.Info("DAEMON", "拒绝切换定时任务:有任务正在运行 %s(%s)", j.Type, j.ID)
		writeError(w, http.StatusConflict, fmt.Sprintf("有任务正在运行(%s),请等待完成后再切换定时任务", j.Type))
		return
	}
	newState := srv.deps.Daemon.Toggle()
	srv.deps.Logger.Info("DAEMON", "Web 请求切换后台模式: %v", newState)
	// 同步更新配置中的 daemon_mode
	srv.deps.Cfg.DaemonMode = newState
	// 持久化到 config.yaml,确保重启后恢复定时运行状态
	if srv.deps.CfgPath != "" {
		if err := config.SaveConfig(srv.deps.Cfg, srv.deps.CfgPath); err != nil {
			srv.deps.Logger.Error("DAEMON", "持久化 daemon_mode 失败: %v", err)
		} else {
			srv.deps.Logger.Info("DAEMON", "daemon_mode=%v 已持久化到 %s", newState, srv.deps.CfgPath)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active": newState,
		"message": func() string {
			if newState {
				return "定时任务已启动"
			}
			return "定时任务已停止"
		}(),
	})
}
