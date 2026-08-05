package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cf-speedtest/updater"
)

// updateStatusHandler GET /api/update/status
// 返回当前版本检查状态(最近一次 manifest、是否有新版本、当前更新流程状态)
// 即使 update_check_enable=false 也会返回(用当前版本填充 latest_version,has_update=false)
func (srv *Server) updateStatusHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Manager == nil {
		// 配置未启用更新检查:返回基础状态(state=idle, has_update=false)
		writeJSON(w, http.StatusOK, updater.StatusResponse{
			CurrentVersion: srv.deps.Version,
			LatestVersion:  srv.deps.Version,
			HasUpdate:      false,
			State:          updater.StateIdle,
			LastCheckError: "更新检查未启用(update_check_enable=false)",
		})
		return
	}
	// Manager 持有当前更新流程状态;Checker 提供版本信息
	writeJSON(w, http.StatusOK, srv.deps.Checker.Status(srv.deps.Manager.State()))
}

// updateCheckHandler POST /api/update/check
// 手动触发一次版本检查(同步执行,完成后返回最新状态)
// 如果当前正在执行更新流程(state != idle && state != failed),返回 409 Conflict
func (srv *Server) updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Manager == nil {
		writeError(w, http.StatusForbidden, "更新检查未启用(update_check_enable=false)")
		return
	}

	// 设置 60s 超时(拉取 version.json 通常 1-3 秒,网络异常时不会卡死)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if _, _, err := srv.deps.Manager.Check(ctx); err != nil {
		// 并发冲突:更新流程进行中
		if errors.Is(err, updater.ErrUpdateInProgress) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		// 其他错误(网络/解析):返回 200 + Status(包含 last_error),前端通过 last_check_error 显示
		srv.deps.Logger.Warn("UPDATE", "手动版本检查失败: %v", err)
	}
	writeJSON(w, http.StatusOK, srv.deps.Checker.Status(srv.deps.Manager.State()))
}

// Restart 实现 updater.Restarter 接口
// Phase 3 中 Manager 在安装完成后调用此方法重启进程
func (srv *Server) Restart() error {
	return srv.restart()
}
