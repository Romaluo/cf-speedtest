package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cf-speedtest/updater"
)

// updateApplyHandler POST /api/update/apply
// 触发一键更新流程,立即返回 202 Accepted,后台异步执行
// 如果当前已有更新进行中,返回 409 Conflict
// 如果更新器未初始化(checker/downloader 缺失),返回 503 Service Unavailable
func (srv *Server) updateApplyHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Manager == nil {
		writeError(w, http.StatusForbidden, "更新检查未启用(update_check_enable=false)")
		return
	}

	if err := srv.deps.Manager.Apply(); err != nil {
		if errors.Is(err, updater.ErrUpdateInProgress) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		// 组件未初始化(checker/downloader 缺失):返回 503,提示配置问题
		if errors.Is(err, updater.ErrCheckerNotInitialized) || errors.Is(err, updater.ErrDownloaderNotInitialized) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("启动更新失败: %v", err))
		return
	}
	srv.deps.Logger.Info("UPDATE", "用户触发了更新流程")
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"ok":      true,
		"message": "更新流程已启动",
	})
}

// updateCancelHandler POST /api/update/cancel
// 取消正在进行的下载(仅 downloading/verifying 状态有效)
// 已完成或已失败的流程调用此方法无效果
func (srv *Server) updateCancelHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Manager == nil {
		writeError(w, http.StatusForbidden, "更新检查未启用(update_check_enable=false)")
		return
	}
	srv.deps.Manager.Cancel()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "取消请求已发送",
	})
}

// updateProgressHandler GET /api/update/progress (SSE)
// 服务端推送更新进度事件,客户端保持连接接收
// 事件类型:
//   - event: progress → 下载/校验/安装中(data 为 ProgressEvent)
//   - event: done → 更新完成(短暂状态,随后进程重启)
//   - event: error → 更新失败(data 包含 error 字段)
//
// 客户端断开连接时自动取消订阅
func (srv *Server) updateProgressHandler(w http.ResponseWriter, r *http.Request) {
	if srv.deps.Manager == nil {
		writeError(w, http.StatusForbidden, "更新检查未启用(update_check_enable=false)")
		return
	}

	// 检查 ResponseWriter 是否支持 Flusher(SSE 必需)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "服务器不支持 SSE")
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 禁用 nginx 缓冲,确保事件实时推送
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 订阅进度事件
	ch := srv.deps.Manager.Subscribe()
	defer srv.deps.Manager.Unsubscribe(ch)

	// 心跳:每 15s 推送一个注释行,防止代理超时关闭连接
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				// channel 已关闭(Unsubscribe 调用)
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				srv.deps.Logger.Warn("UPDATE", "SSE 推送失败: %v", err)
				return
			}
			flusher.Flush()
			// 终态事件推送后关闭连接
			if ev.State == updater.StateFailed || ev.State == updater.StateDone {
				return
			}
		case <-heartbeat.C:
			// SSE 注释行(以 : 开头),客户端忽略,仅用于保活
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-ctx.Done():
			// 客户端断开连接
			return
		}
	}
}

// writeSSEEvent 写入一个 SSE 事件
// 事件名根据 state 自动选择:progress / done / error
func writeSSEEvent(w http.ResponseWriter, ev updater.ProgressEvent) error {
	jsonData, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	eventName := sseEventName(ev.State)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, jsonData)
	return nil
}

// sseEventName 根据状态选择 SSE 事件名
func sseEventName(s updater.State) string {
	switch s {
	case updater.StateFailed:
		return "error"
	case updater.StateDone, updater.StateIdle:
		// StateIdle 在更新流程中作为"完成"信号(Phase 2:下载+校验完成;Phase 3:重启完成)
		return "done"
	default:
		return "progress"
	}
}
