package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"cf-speedtest/cleanup"
	"cf-speedtest/collector"
	"cf-speedtest/config"
	"cf-speedtest/geo"
	"cf-speedtest/log"
	"cf-speedtest/repository"
	"cf-speedtest/updater"
)

//go:embed static/*
var staticFS embed.FS

// Deps Web 服务依赖项
type Deps struct {
	Cfg       *config.Config
	CfgPath   string
	DB        *repository.DB
	Resolver  *geo.Resolver
	Logger    *log.Logger
	Version   string
	Daemon    *DaemonControl
	CIDRStats *collector.CIDRStats
	Checker   *updater.Checker // 版本检查器(可为 nil,表示禁用更新检查)
	Manager   *updater.Manager // 更新管理器(可为 nil,与 Checker 同时为 nil 或同时非 nil)
}

// Server Web Dashboard 服务
type Server struct {
	deps       Deps
	sessions   *sessionStore
	jobs       *jobTracker
	startAt    time.Time
	triggerMu  sync.Mutex // 串行化触发任务，避免并发测速/推送
	httpServer *http.Server
	cleaner    *cleanup.Cleaner // 资源清理器
}

// NewServer 创建 Web 服务实例
func NewServer(deps Deps) *Server {
	return &Server{
		deps:     deps,
		sessions: newSessionStore(deps.Cfg.WebSessionTTL),
		jobs:     newJobTracker(),
		startAt:  time.Now(),
		cleaner:  cleanup.New(deps.Cfg, deps.Logger),
	}
}

// Start 启动 HTTP 服务（阻塞）
func (srv *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", srv.deps.Cfg.WebHost, srv.deps.Cfg.WebPort)
	mux := srv.routes()

	srv.deps.Logger.Info("WEB", "Web Dashboard 启动: http://%s", addr)
	fmt.Printf("[INFO] Web Dashboard 启动: http://%s (用户: %s)\n", addr, srv.deps.Cfg.WebUsername)

	srv.httpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.httpServer.ListenAndServe()
}

// Shutdown 优雅关闭 HTTP 服务
func (srv *Server) Shutdown(ctx context.Context) error {
	if srv.httpServer != nil {
		return srv.httpServer.Shutdown(ctx)
	}
	return nil
}

// routes 构建路由表
func (srv *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/health", srv.healthHandler)
	mux.HandleFunc("POST /api/login", srv.loginHandler)
	mux.HandleFunc("POST /api/logout", srv.logoutHandler)
	mux.HandleFunc("GET /api/auth/status", srv.authStatusHandler)

	// 需鉴权接口
	auth := srv.authMiddleware

	// 测速结果
	mux.HandleFunc("GET /api/results", auth(srv.listResultsHandler))
	mux.HandleFunc("GET /api/results/top", auth(srv.topResultsHandler))
	mux.HandleFunc("DELETE /api/results", auth(srv.clearResultsHandler))
	mux.HandleFunc("DELETE /api/results/{ip}/{port}", auth(srv.deleteResultHandler))

	// 统计与维度
	mux.HandleFunc("GET /api/stats", auth(srv.statsHandler))
	mux.HandleFunc("GET /api/dimensions", auth(srv.dimensionsHandler))

	// 配置
	mux.HandleFunc("GET /api/config", auth(srv.getConfigHandler))
	mux.HandleFunc("PUT /api/config", auth(srv.setConfigHandler))
	mux.HandleFunc("POST /api/config/test", auth(srv.testConfigHandler))

	// 推送器状态
	mux.HandleFunc("GET /api/pushers/status", auth(srv.pushersStatusHandler))

	// 触发任务
	mux.HandleFunc("POST /api/trigger/benchmark", auth(srv.triggerBenchmarkHandler))
	mux.HandleFunc("POST /api/trigger/push", auth(srv.triggerPushHandler))
	mux.HandleFunc("GET /api/jobs", auth(srv.listJobsHandler))
	mux.HandleFunc("GET /api/jobs/{id}", auth(srv.getJobHandler))
	mux.HandleFunc("POST /api/jobs/cancel", auth(srv.cancelJobHandler))

	// 日志
	mux.HandleFunc("GET /api/logs", auth(srv.logsHandler))

	// 系统信息
	mux.HandleFunc("GET /api/system", auth(srv.systemHandler))
	mux.HandleFunc("POST /api/system/restart", auth(srv.restartHandler))

	mux.HandleFunc("GET /api/daemon/status", auth(srv.daemonStatusHandler))
	mux.HandleFunc("POST /api/daemon/toggle", auth(srv.daemonToggleHandler))

	// 自动更新（版本检查 + 下载/安装/进度）
	mux.HandleFunc("GET /api/update/status", auth(srv.updateStatusHandler))
	mux.HandleFunc("POST /api/update/check", auth(srv.updateCheckHandler))
	mux.HandleFunc("POST /api/update/apply", auth(srv.updateApplyHandler))
	mux.HandleFunc("GET /api/update/progress", auth(srv.updateProgressHandler))
	mux.HandleFunc("POST /api/update/cancel", auth(srv.updateCancelHandler))

	// 数据库管理（清空/备份/恢复）
	mux.HandleFunc("POST /api/database/clear", auth(srv.clearDatabaseHandler))
	mux.HandleFunc("POST /api/database/backup", auth(srv.backupDatabaseHandler))
	mux.HandleFunc("POST /api/database/restore", auth(srv.restoreDatabaseHandler))
	mux.HandleFunc("GET /api/database/backups", auth(srv.listBackupsHandler))
	mux.HandleFunc("DELETE /api/database/backups", auth(srv.deleteBackupHandler))

	// 静态资源（前端）
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		srv.deps.Logger.Error("WEB", "加载静态资源失败: %v", err)
	} else {
		fileServer := http.FileServer(http.FS(staticSub))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// SPA 兜底: 非文件请求返回 index.html
			path := strings.TrimPrefix(r.URL.Path, "/")
			if path == "" {
				path = "index.html"
			}
			if _, err := fs.Stat(staticSub, path); err != nil {
				r.URL.Path = "/"
			}
			// 添加 no-cache 头，确保浏览器总是获取最新版本
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			fileServer.ServeHTTP(w, r)
		})
	}

	// 请求日志中间件
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			srv.deps.Logger.Debug("WEB", "%s %s", r.Method, r.URL.Path)
		}
		mux.ServeHTTP(w, r)
	})
}

// ===== 通用辅助函数 =====

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
