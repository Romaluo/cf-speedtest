package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// backupDir 返回备份目录路径（数据库文件所在目录的 backups 子目录）
func (srv *Server) backupDir() string {
	return filepath.Join(filepath.Dir(srv.deps.Cfg.DBPath), "backups")
}

// getCurrentUser 从请求中获取当前登录用户名
func (srv *Server) getCurrentUser(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "unknown"
	}
	if u, ok := srv.sessions.validate(c.Value); ok {
		return u
	}
	return "unknown"
}

// backupDatabaseHandler POST /api/database/backup
// 手动创建数据库备份
func (srv *Server) backupDatabaseHandler(w http.ResponseWriter, r *http.Request) {
	user := srv.getCurrentUser(r)
	srv.deps.Logger.Info("DB", "用户 %s 发起手动备份", user)
	backupPath, backupCount, err := srv.deps.DB.BackupDB(srv.backupDir())
	if err != nil {
		srv.deps.Logger.Error("DB", "手动备份失败: %v", err)
		writeError(w, http.StatusInternalServerError, "备份失败: "+err.Error())
		return
	}
	srv.deps.Logger.Info("DB", "手动备份完成: %s (含 %d 条记录)", backupPath, backupCount)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":           true,
		"backup_path":  backupPath,
		"backup_count": backupCount,
		"backup_time":  time.Now().Format(time.RFC3339),
		"message":      "备份成功",
	})
}

// clearDatabaseHandler POST /api/database/clear
// 清空数据库（含自动备份 + 5分钟恢复窗口）
func (srv *Server) clearDatabaseHandler(w http.ResponseWriter, r *http.Request) {
	user := srv.getCurrentUser(r)

	// 解析请求体（可选确认标志）
	var req struct {
		Confirm bool `json:"confirm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "需确认清空操作")
		return
	}

	// 检查是否有任务正在运行
	if j := srv.jobs.current(); j != nil {
		writeError(w, http.StatusConflict, "有任务正在运行，请等待完成后再清空")
		return
	}

	// 步骤1: 自动备份
	srv.deps.Logger.Info("DB", "用户 %s 发起清空数据库操作，开始自动备份", user)
	backupPath, backupCount, err := srv.deps.DB.BackupDB(srv.backupDir())
	if err != nil {
		srv.deps.Logger.Error("DB", "清空前备份失败: %v", err)
		writeError(w, http.StatusInternalServerError, "备份失败: "+err.Error())
		return
	}
	srv.deps.Logger.Info("DB", "备份完成: %s (含 %d 条记录)", backupPath, backupCount)

	// 步骤2: 清空数据
	deleted, err := srv.deps.DB.ClearAllData()
	if err != nil {
		srv.deps.Logger.Error("DB", "清空数据失败: %v (备份文件: %s)", err, backupPath)
		writeError(w, http.StatusInternalServerError, "清空失败: "+err.Error())
		return
	}

	// 记录操作日志
	now := time.Now()
	restoreDeadline := now.Add(5 * time.Minute)
	srv.deps.Logger.Info("DB", "清空数据库成功 | 操作人: %s | 时间: %s | 删除: %d 条 | 备份: %s",
		user, now.Format("2006-01-02 15:04:05"), deleted, backupPath)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":                  true,
		"deleted":             deleted,
		"backup_path":         backupPath,
		"backup_count":        backupCount,
		"backup_time":         now.Format(time.RFC3339),
		"restore_deadline":    restoreDeadline.Format(time.RFC3339),
		"restore_deadline_ts": restoreDeadline.Unix(),
		"message":             "数据库已清空，5分钟内可恢复",
	})
}

// restoreDatabaseHandler POST /api/database/restore
// 从备份文件恢复数据
func (srv *Server) restoreDatabaseHandler(w http.ResponseWriter, r *http.Request) {
	user := srv.getCurrentUser(r)

	var req struct {
		BackupPath string `json:"backup_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求参数错误")
		return
	}
	if req.BackupPath == "" {
		writeError(w, http.StatusBadRequest, "需指定备份文件路径")
		return
	}

	// 安全检查：只允许恢复 backups 目录下的文件
	backupDir := srv.backupDir()
	// 使用带分隔符的前缀检查，避免 /data/backups_evil 之类旁路攻击
	if !strings.HasPrefix(req.BackupPath, backupDir+string(filepath.Separator)) &&
		req.BackupPath != backupDir {
		writeError(w, http.StatusBadRequest, "仅允许恢复备份目录中的文件")
		return
	}

	// 验证备份文件存在
	info, err := srv.deps.DB.ListBackups(backupDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询备份失败: "+err.Error())
		return
	}
	backupExists := false
	for _, b := range info {
		if b.Path == req.BackupPath {
			backupExists = true
			break
		}
	}
	if !backupExists {
		writeError(w, http.StatusNotFound, "备份文件不存在")
		return
	}

	// 检查是否有任务正在运行
	if j := srv.jobs.current(); j != nil {
		writeError(w, http.StatusConflict, "有任务正在运行，请等待完成后再恢复")
		return
	}

	// 执行恢复
	srv.deps.Logger.Info("DB", "用户 %s 发起恢复操作，从 %s 恢复", user, req.BackupPath)
	restored, err := srv.deps.DB.RestoreFromBackup(req.BackupPath)
	if err != nil {
		srv.deps.Logger.Error("DB", "恢复失败: %v", err)
		writeError(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}

	srv.deps.Logger.Info("DB", "恢复成功 | 操作人: %s | 时间: %s | 恢复: %d 条 | 来源: %s",
		user, time.Now().Format("2006-01-02 15:04:05"), restored, req.BackupPath)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"restored": restored,
		"message":  "数据恢复成功",
	})
}

// listBackupsHandler GET /api/database/backups
// 列出所有备份文件（含可恢复状态）
func (srv *Server) listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	backups, err := srv.deps.DB.ListBackups(srv.backupDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询备份列表失败: "+err.Error())
		return
	}

	// 标注备份类型
	type backupView struct {
		Path      string    `json:"path"`
		Size      int64     `json:"size"`
		CreatedAt time.Time `json:"created_at"`
		Type      string    `json:"type"` // "auto" 自动备份 / "manual" 手动备份
	}

	var views []backupView
	for _, b := range backups {
		name := filepath.Base(b.Path)
		v := backupView{
			Path:      b.Path,
			Size:      b.Size,
			CreatedAt: b.CreatedAt,
			Type:      "manual",
		}
		if name == "speedtest_auto_backup.db" {
			v.Type = "auto"
		}
		views = append(views, v)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"backups":    views,
		"backup_dir": srv.backupDir(),
	})
}

// deleteBackupHandler DELETE /api/database/backups
// 删除指定备份文件
func (srv *Server) deleteBackupHandler(w http.ResponseWriter, r *http.Request) {
	user := srv.getCurrentUser(r)

	backupPath := r.URL.Query().Get("path")
	if backupPath == "" {
		writeError(w, http.StatusBadRequest, "需指定备份文件路径")
		return
	}

	// 安全检查
	backupDir := srv.backupDir()
	if !strings.HasPrefix(backupPath, backupDir+string(filepath.Separator)) &&
		backupPath != backupDir {
		writeError(w, http.StatusBadRequest, "仅允许删除备份目录中的文件")
		return
	}

	if err := srv.deps.DB.DeleteBackup(backupPath); err != nil {
		writeError(w, http.StatusInternalServerError, "删除备份失败: "+err.Error())
		return
	}

	srv.deps.Logger.Info("DB", "用户 %s 删除备份文件: %s", user, backupPath)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}
