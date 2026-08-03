package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// restartHandler POST /api/system/restart
// 先返回 HTTP 200，然后在后台 goroutine 中执行重启
func (srv *Server) restartHandler(w http.ResponseWriter, r *http.Request) {
	srv.deps.Logger.Info("WEB", "收到系统重启请求")

	// 先响应客户端
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "系统即将重启，请稍候...",
	})

	// 延迟 500ms 执行重启，确保响应已发送
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := srv.restart(); err != nil {
			srv.deps.Logger.Error("WEB", "重启失败: %v", err)
			fmt.Fprintf(os.Stderr, "[ERROR] 重启失败: %v\n", err)
			// 重启失败则不退出，保持运行
		}
	}()
}

// restart 执行进程重启
// systemd 环境下使用 systemctl restart（避免 fork-exec 与 systemd 冲突）
// 非 systemd 环境使用原有 fork-exec 方式
func (srv *Server) restart() error {
	// systemd 环境检测: INVOCATION_ID 由 systemd 为每个服务设置
	if os.Getenv("INVOCATION_ID") != "" {
		return srv.restartViaSystemd()
	}
	return srv.restartViaForkExec()
}

// restartViaSystemd 通过 systemctl restart 重启服务
// systemd 会向当前进程发送 SIGTERM，触发信号处理器的优雅退出逻辑
func (srv *Server) restartViaSystemd() error {
	srv.deps.Logger.Info("WEB", "systemd 环境检测到，使用 systemctl restart 重启服务")
	fmt.Println("[INFO] 正在通过 systemd 重启服务...")

	// 启动 systemctl restart 命令（分离进程，不等待完成）
	// systemctl 会向当前进程发送 SIGTERM，由 runDaemon 的信号处理器优雅退出
	cmd := exec.Command("systemctl", "--user", "restart", "cf-speedtest.service")
	cmd.Env = os.Environ() // 继承环境（DBUS_SESSION_BUS_ADDRESS 等）
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 systemctl restart 失败: %w", err)
	}
	// 释放子进程资源，让 systemctl 独立运行
	_ = cmd.Process.Release()
	return nil
}

// restartViaForkExec 非 systemd 环境的 fork-exec 重启方式
//  1. 优雅关闭 HTTP 服务（给现有请求 5s 处理时间）
//  2. fork-exec 启动新的 cf-speedtest 进程
//  3. 等待新进程成功绑定端口（带重试）
//  4. 退出旧进程
func (srv *Server) restartViaForkExec() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Step 1: 优雅关闭 HTTP 服务
	srv.deps.Logger.Info("WEB", "正在关闭 HTTP 服务...")
	fmt.Println("[INFO] 正在重启系统...")

	if srv.httpServer != nil {
		if err := srv.httpServer.Shutdown(ctx); err != nil {
			srv.deps.Logger.Error("WEB", "HTTP 服务关闭超时: %v", err)
			// 继续执行重启，即使关闭超时
		}
	}

	// 等待端口释放
	time.Sleep(500 * time.Millisecond)

	// Step 2: 获取当前可执行文件路径和参数
	exe, err := os.Executable()
	if err != nil {
		// 退化: 使用 os.Args[0]
		exe = os.Args[0]
	}
	args := os.Args[1:]

	srv.deps.Logger.Info("WEB", "启动新进程: %s %v", exe, args)

	// Step 3: 启动新进程
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新进程失败: %w", err)
	}

	// Step 4: 验证新进程成功绑定端口（最多重试 5 次）
	port := fmt.Sprintf("%s:%d", srv.deps.Cfg.WebHost, srv.deps.Cfg.WebPort)
	var bindOK bool
	for i := 0; i < 5; i++ {
		time.Sleep(1 * time.Second)
		if waitPortAvailable(port) {
			bindOK = true
			break
		}
	}

	if !bindOK {
		srv.deps.Logger.Error("WEB", "新进程未能在端口 %s 完成绑定，强制退出旧进程", port)
		fmt.Fprintf(os.Stderr, "[WARN] 新进程端口绑定超时，旧进程退出中...\n")
	} else {
		srv.deps.Logger.Info("WEB", "新进程已成功绑定端口 %s，旧进程退出", port)
		fmt.Printf("[INFO] 系统重启成功 (新进程 PID: %d)\n", cmd.Process.Pid)
	}

	// Step 5: 退出旧进程
	os.Exit(0)
	return nil // 不会执行到
}

// waitPortAvailable 检查指定端口是否有进程在监听
func waitPortAvailable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
