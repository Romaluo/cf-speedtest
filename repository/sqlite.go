package repository

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cf-speedtest/model"

	_ "modernc.org/sqlite"
)

const (
	batchSize = 500
)

type DB struct {
	conn   *sql.DB
	dbPath string
}

// BackupInfo 备份文件信息
type BackupInfo struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	Count     int64     `json:"count"` // 备份时的记录数
}

func NewDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// 连接池配置: SQLite WAL 模式下写操作必须串行
	// SetMaxOpenConns(1) 防止并发写入导致 "database is locked" 错误
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(time.Hour)

	// WAL 模式: 读写并发，显著提升并发性能
	// synchronous=NORMAL: WAL 模式下安全且性能最优（比 FULL 快数倍）
	// busy_timeout=5000: 锁等待 5 秒，避免瞬时锁竞争失败
	// cache_size=-64000: 64MB 页缓存，提升读性能
	// temp_store=MEMORY: 临时表存储在内存中
	// journal_size_limit=67108864: WAL 文件上限 64MB
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA cache_size=-64000",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA journal_size_limit=67108864",
	}

	for _, pragma := range pragmas {
		if _, err := conn.Exec(pragma); err != nil {
			conn.Close()
			return nil, fmt.Errorf("PRAGMA %s 执行失败: %w", pragma, err)
		}
	}

	// 验证 WAL 模式是否成功激活
	var walMode string
	if err := conn.QueryRow("PRAGMA journal_mode").Scan(&walMode); err != nil {
		conn.Close()
		return nil, fmt.Errorf("查询 WAL 模式失败: %w", err)
	}
	if walMode != "wal" {
		conn.Close()
		return nil, fmt.Errorf("WAL 模式激活失败，当前模式: %s", walMode)
	}

	// 自动迁移: 检查旧表结构（ip 单独主键）并迁移为 (ip, port) 复合主键
	if err := migrateSchema(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return &DB{conn: conn, dbPath: dbPath}, nil
}

// migrateSchema 迁移旧版表结构（ip 单独主键 → ip+port 复合主键）
func migrateSchema(conn *sql.DB) error {
	// 检查表是否存在
	var tableExists int
	if err := conn.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ip_results'",
	).Scan(&tableExists); err != nil {
		return err
	}

	if tableExists == 0 {
		// 新表创建
		return createTable(conn)
	}

	// 检查当前主键结构
	pkInfo, err := conn.Query("PRAGMA index_list('ip_results')")
	if err != nil {
		return err
	}
	defer pkInfo.Close()

	needsMigration := false
	for pkInfo.Next() {
		var seq, origin int
		var name, unique string
		if err := pkInfo.Scan(&seq, &name, &unique, &origin); err != nil {
			continue
		}
		if origin == 1 { // origin=1 表示主键
			// 获取主键列
			pkColumns, err := conn.Query(fmt.Sprintf("PRAGMA index_info('%s')", name))
			if err != nil {
				continue
			}
			var colCount int
			for pkColumns.Next() {
				colCount++
			}
			pkColumns.Close()
			if colCount == 1 {
				needsMigration = true
			}
		}
	}

	if !needsMigration {
		return nil
	}

	// 执行迁移: 创建新表 → 迁移数据 → 删除旧表 → 重命名
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 重命名旧表
	if _, err := tx.Exec("ALTER TABLE ip_results RENAME TO ip_results_old"); err != nil {
		return err
	}

	// 2. 创建新表
	if err := createTableWithTx(tx); err != nil {
		return err
	}

	// 3. 迁移数据（设置默认端口 443）
	if _, err := tx.Exec(`
		INSERT INTO ip_results (ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score, updated_at)
		SELECT ip, 443, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score, updated_at
		FROM ip_results_old
	`); err != nil {
		return err
	}

	// 4. 删除旧表
	if _, err := tx.Exec("DROP TABLE ip_results_old"); err != nil {
		return err
	}

	return tx.Commit()
}

func createTable(conn *sql.DB) error {
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS ip_results (
		ip TEXT NOT NULL,
		port INTEGER NOT NULL,
		country_code TEXT,
		isp TEXT,
		tcp_latency_avg INTEGER,
		tcp_loss_rate REAL,
		http_latency_avg INTEGER,
		download_speed REAL,
		score REAL,
		updated_at DATETIME,
		PRIMARY KEY(ip, port)
	);`

	if _, err := conn.Exec(createTableSQL); err != nil {
		return err
	}

	createIndexSQL := `CREATE INDEX IF NOT EXISTS idx_ip_results_updated_at ON ip_results(updated_at);`
	conn.Exec(createIndexSQL)

	return nil
}

func createTableWithTx(tx *sql.Tx) error {
	createTableSQL := `
	CREATE TABLE ip_results (
		ip TEXT NOT NULL,
		port INTEGER NOT NULL,
		country_code TEXT,
		isp TEXT,
		tcp_latency_avg INTEGER,
		tcp_loss_rate REAL,
		http_latency_avg INTEGER,
		download_speed REAL,
		score REAL,
		updated_at DATETIME,
		PRIMARY KEY(ip, port)
	);`

	if _, err := tx.Exec(createTableSQL); err != nil {
		return err
	}

	createIndexSQL := `CREATE INDEX idx_ip_results_updated_at ON ip_results(updated_at);`
	tx.Exec(createIndexSQL)

	return nil
}

// GetValidIPs 获取未过期的 IP 集合（key 为 "ip:port"）
func (db *DB) GetValidIPs(expireDuration time.Duration) (map[string]bool, error) {
	validIPs := make(map[string]bool)
	threshold := time.Now().Add(-expireDuration)

	rows, err := db.conn.Query("SELECT ip, port FROM ip_results WHERE updated_at > ?", threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var ip string
		var port int
		if err := rows.Scan(&ip, &port); err != nil {
			return nil, err
		}
		validIPs[fmt.Sprintf("%s:%d", ip, port)] = true
	}
	return validIPs, nil
}

// BatchUpsert 批量写入（分块事务，每 batchSize 条为一个事务）
func (db *DB) BatchUpsert(results []model.IPResult) error {
	// 过滤错误结果 — 内存优化:预分配容量(大多数结果有效)
	valid := make([]model.IPResult, 0, len(results))
	for _, r := range results {
		if r.Err == nil {
			valid = append(valid, r)
		}
	}

	if len(valid) == 0 {
		return nil
	}

	stmtSQL := `
	INSERT INTO ip_results (ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(ip, port) DO UPDATE SET 
		country_code=excluded.country_code,
		isp=excluded.isp,
		tcp_latency_avg=excluded.tcp_latency_avg,
		tcp_loss_rate=excluded.tcp_loss_rate,
		http_latency_avg=excluded.http_latency_avg,
		download_speed=excluded.download_speed,
		score=excluded.score,
		updated_at=excluded.updated_at;`

	now := time.Now()

	// 分块处理，避免单事务过大
	for i := 0; i < len(valid); i += batchSize {
		end := i + batchSize
		if end > len(valid) {
			end = len(valid)
		}
		chunk := valid[i:end]

		tx, err := db.conn.Begin()
		if err != nil {
			return fmt.Errorf("开启事务失败: %w", err)
		}

		stmt, err := tx.Prepare(stmtSQL)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("预编译语句失败: %w", err)
		}

		for _, r := range chunk {
			_, err := stmt.Exec(
				r.IP,
				r.Port,
				r.CountryCode,
				r.ISP,
				r.TCPLatencyAvg.Milliseconds(),
				r.TCPLossRate,
				r.HTTPLatencyAvg.Milliseconds(),
				r.DownloadSpeed,
				r.Score,
				now,
			)
			if err != nil {
				stmt.Close()
				tx.Rollback()
				return fmt.Errorf("写入 %s:%d 失败: %w", r.IP, r.Port, err)
			}
		}

		stmt.Close()
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交事务失败: %w", err)
		}
	}

	return nil
}

// GetTopResults 获取评分最高的前 N 条结果
func (db *DB) GetTopResults(topN int) ([]model.IPResult, error) {
	rows, err := db.conn.Query(`
		SELECT ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score 
		FROM ip_results 
		ORDER BY score DESC 
		LIMIT ?`, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 内存优化:已知 LIMIT = topN,预分配容量
	results := make([]model.IPResult, 0, topN)
	for rows.Next() {
		var r model.IPResult
		var tcpLatency, httpLatency int64
		err := rows.Scan(
			&r.IP,
			&r.Port,
			&r.CountryCode,
			&r.ISP,
			&tcpLatency,
			&r.TCPLossRate,
			&httpLatency,
			&r.DownloadSpeed,
			&r.Score,
		)
		if err != nil {
			return nil, err
		}
		r.TCPLatencyAvg = time.Duration(tcpLatency) * time.Millisecond
		r.HTTPLatencyAvg = time.Duration(httpLatency) * time.Millisecond
		results = append(results, r)
	}
	return results, nil
}

// CleanupOldData 清理过期数据
func (db *DB) CleanupOldData(retentionDays int) (int64, error) {
	threshold := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	result, err := db.conn.Exec("DELETE FROM ip_results WHERE updated_at < ?", threshold)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Vacuum 压缩数据库文件，回收未使用的磁盘空间
// 执行 VACUUM 命令重建数据库文件，同时清理 WAL 文件
func (db *DB) Vacuum() error {
	// 1. 将 WAL 日志合并到主数据库
	if _, err := db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("WAL checkpoint 失败: %w", err)
	}
	// 2. VACUUM 压缩数据库文件
	if _, err := db.conn.Exec("VACUUM"); err != nil {
		return fmt.Errorf("VACUUM 失败: %w", err)
	}
	return nil
}

// BackupDB 备份数据库到指定目录，返回备份文件路径和备份时记录数
func (db *DB) BackupDB(backupDir string) (string, int64, error) {
	// 确保备份目录存在
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", 0, fmt.Errorf("创建备份目录失败: %w", err)
	}

	// 先将 WAL 日志合并到主数据库，确保备份完整
	if _, err := db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", 0, fmt.Errorf("WAL checkpoint 失败: %w", err)
	}

	// 获取当前记录数
	count, _ := db.Count()

	// 生成备份文件名（带时间戳）
	backupName := fmt.Sprintf("speedtest_backup_%s.db", time.Now().Format("20060102_150405"))
	backupPath := filepath.Join(backupDir, backupName)

	// 复制数据库文件
	src, err := os.Open(db.dbPath)
	if err != nil {
		return "", 0, fmt.Errorf("打开数据库文件失败: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(backupPath)
	if err != nil {
		return "", 0, fmt.Errorf("创建备份文件失败: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", 0, fmt.Errorf("复制数据库文件失败: %w", err)
	}

	return backupPath, count, nil
}

// AutoBackup 自动备份（覆盖式，只保留最近一份）
// 用于测速入库前自动保存当前数据库状态，文件名固定为 speedtest_auto_backup.db
func (db *DB) AutoBackup(backupDir string) (string, int64, error) {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", 0, fmt.Errorf("创建备份目录失败: %w", err)
	}
	if _, err := db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", 0, fmt.Errorf("WAL checkpoint 失败: %w", err)
	}
	count, _ := db.Count()
	// 固定文件名，覆盖旧备份（只保留最近一份）
	backupPath := filepath.Join(backupDir, "speedtest_auto_backup.db")
	src, err := os.Open(db.dbPath)
	if err != nil {
		return "", 0, fmt.Errorf("打开数据库文件失败: %w", err)
	}
	defer src.Close()
	dst, err := os.Create(backupPath)
	if err != nil {
		return "", 0, fmt.Errorf("创建备份文件失败: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return "", 0, fmt.Errorf("复制数据库文件失败: %w", err)
	}
	return backupPath, count, nil
}

// ClearAllData 清空所有数据，返回删除的行数
func (db *DB) ClearAllData() (int64, error) {
	// 获取当前记录数
	var count int64
	count, _ = db.Count()

	// 删除所有数据
	result, err := db.conn.Exec("DELETE FROM ip_results")
	if err != nil {
		return 0, fmt.Errorf("清空数据失败: %w", err)
	}
	deleted, _ := result.RowsAffected()

	// VACUUM 回收空间
	db.conn.Exec("VACUUM")

	if deleted > 0 {
		return deleted, nil
	}
	return count, nil
}

// RestoreFromBackup 从备份文件恢复数据
// 使用 ATTACH DATABASE 方式，不需要关闭当前连接
func (db *DB) RestoreFromBackup(backupPath string) (int64, error) {
	// 检查备份文件是否存在
	if _, err := os.Stat(backupPath); err != nil {
		return 0, fmt.Errorf("备份文件不存在: %w", err)
	}

	// 清空当前数据
	if _, err := db.ClearAllData(); err != nil {
		return 0, fmt.Errorf("清空当前数据失败: %w", err)
	}

	// 附加备份数据库
	attachName := fmt.Sprintf("backup_restore_%d", time.Now().UnixNano())
	_, err := db.conn.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS %s", backupPath, attachName))
	if err != nil {
		return 0, fmt.Errorf("附加备份数据库失败: %w", err)
	}
	defer db.conn.Exec(fmt.Sprintf("DETACH DATABASE %s", attachName))

	// 从备份恢复数据
	_, err = db.conn.Exec(fmt.Sprintf(`
		INSERT INTO ip_results (ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score, updated_at)
		SELECT ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score, updated_at
		FROM %s.ip_results
	`, attachName))
	if err != nil {
		return 0, fmt.Errorf("恢复数据失败: %w", err)
	}

	// 返回恢复的记录数
	count, _ := db.Count()
	return count, nil
}

// ListBackups 列出指定目录中的所有备份文件
func (db *DB) ListBackups(backupDir string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}

	var backups []BackupInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// 只匹配备份文件（手动/清空前: speedtest_backup_*.db，自动: speedtest_auto_backup.db）
		if (!strings.HasPrefix(name, "speedtest_backup_") && name != "speedtest_auto_backup.db") || filepath.Ext(name) != ".db" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		backups = append(backups, BackupInfo{
			Path:      filepath.Join(backupDir, name),
			Size:      info.Size(),
			CreatedAt: info.ModTime(),
		})
	}

	// 按时间倒序排列（最新在前）
	for i := 0; i < len(backups)-1; i++ {
		for j := i + 1; j < len(backups); j++ {
			if backups[i].CreatedAt.Before(backups[j].CreatedAt) {
				backups[i], backups[j] = backups[j], backups[i]
			}
		}
	}

	return backups, nil
}

// DeleteBackup 删除指定备份文件
func (db *DB) DeleteBackup(backupPath string) error {
	return os.Remove(backupPath)
}

// Count 获取当前数据库中的记录总数
func (db *DB) Count() (int64, error) {
	var count int64
	err := db.conn.QueryRow("SELECT COUNT(*) FROM ip_results").Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// GetAllIPs 获取数据库中所有 IP:Port 组合，用于全量重测
func (db *DB) GetAllIPs() ([]model.Task, error) {
	rows, err := db.conn.Query("SELECT ip, port, country_code FROM ip_results GROUP BY ip, port")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var ip string
		var port int
		var countryCode string
		if err := rows.Scan(&ip, &port, &countryCode); err != nil {
			continue
		}
		tasks = append(tasks, model.Task{IP: ip, Port: port, CountryCode: countryCode})
	}
	return tasks, nil
}

// GetIPsByPorts 获取指定端口列表内的去重 IP:Port 任务列表
func (db *DB) GetIPsByPorts(ports []int) ([]model.Task, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ports))
	args := make([]interface{}, len(ports))
	for i, p := range ports {
		placeholders[i] = "?"
		args[i] = p
	}
	query := fmt.Sprintf("SELECT ip, port, country_code FROM ip_results WHERE port IN (%s) GROUP BY ip, port", strings.Join(placeholders, ","))
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var ip string
		var port int
		var countryCode string
		if err := rows.Scan(&ip, &port, &countryCode); err != nil {
			continue
		}
		tasks = append(tasks, model.Task{IP: ip, Port: port, CountryCode: countryCode})
	}
	return tasks, nil
}

// GetAllIPsBefore 获取 updated_at <= before 的去重 IP:Port 任务列表
// 用于采集后推送时跳过刚采集的IP，仅重测历史IP
func (db *DB) GetAllIPsBefore(before time.Time) ([]model.Task, error) {
	rows, err := db.conn.Query("SELECT ip, port, country_code FROM ip_results WHERE updated_at <= ? GROUP BY ip, port", before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var ip string
		var port int
		var countryCode string
		if err := rows.Scan(&ip, &port, &countryCode); err != nil {
			continue
		}
		tasks = append(tasks, model.Task{IP: ip, Port: port, CountryCode: countryCode})
	}
	return tasks, nil
}

// GetIPsByPortsBefore 获取指定端口列表内、updated_at <= before 的去重 IP:Port 任务列表
func (db *DB) GetIPsByPortsBefore(ports []int, before time.Time) ([]model.Task, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ports))
	args := make([]interface{}, len(ports))
	for i, p := range ports {
		placeholders[i] = "?"
		args[i] = p
	}
	args = append(args, before)
	query := fmt.Sprintf("SELECT ip, port, country_code FROM ip_results WHERE port IN (%s) AND updated_at <= ? GROUP BY ip, port", strings.Join(placeholders, ","))
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var ip string
		var port int
		var countryCode string
		if err := rows.Scan(&ip, &port, &countryCode); err != nil {
			continue
		}
		tasks = append(tasks, model.Task{IP: ip, Port: port, CountryCode: countryCode})
	}
	return tasks, nil
}

// DeleteByPortsNotIn 删除不在指定端口列表中的所有记录，返回删除行数
// 用于测速前预过滤，清除非用户配置端口的旧数据
func (db *DB) DeleteByPortsNotIn(ports []int) (int64, error) {
	if len(ports) == 0 {
		return db.DeleteAll()
	}
	placeholders := make([]string, len(ports))
	args := make([]interface{}, len(ports))
	for i, p := range ports {
		placeholders[i] = "?"
		args[i] = p
	}
	query := fmt.Sprintf("DELETE FROM ip_results WHERE port NOT IN (%s)", strings.Join(placeholders, ","))
	result, err := db.conn.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CountByPortsNotIn 统计不在指定端口列表中的记录数
// 用于清理后验证数据库中是否仍存在非配置端口数据
func (db *DB) CountByPortsNotIn(ports []int) (int64, error) {
	if len(ports) == 0 {
		return db.Count()
	}
	placeholders := make([]string, len(ports))
	args := make([]interface{}, len(ports))
	for i, p := range ports {
		placeholders[i] = "?"
		args[i] = p
	}
	query := fmt.Sprintf("SELECT COUNT(*) FROM ip_results WHERE port NOT IN (%s)", strings.Join(placeholders, ","))
	var count int64
	err := db.conn.QueryRow(query, args...).Scan(&count)
	return count, err
}

// DeleteZeroSpeed 删除下载速度为0的记录（数据清洗）
func (db *DB) DeleteZeroSpeed() (int64, error) {
	res, err := db.conn.Exec("DELETE FROM ip_results WHERE download_speed <= 0")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetTopResultsByPorts 获取指定端口列表中评分最高的前 N 条结果
func (db *DB) GetTopResultsByPorts(topN int, ports []int) ([]model.IPResult, error) {
	if len(ports) == 0 {
		return db.GetTopResults(topN)
	}
	placeholders := make([]string, len(ports))
	args := make([]interface{}, len(ports))
	for i, p := range ports {
		placeholders[i] = "?"
		args[i] = p
	}
	args = append(args, topN)
	query := fmt.Sprintf(`
		SELECT ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate, http_latency_avg, download_speed, score
		FROM ip_results
		WHERE port IN (%s)
		ORDER BY score DESC
		LIMIT ?`, strings.Join(placeholders, ","))

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 内存优化:已知 LIMIT = topN,预分配容量
	results := make([]model.IPResult, 0, topN)
	for rows.Next() {
		var r model.IPResult
		var tcpLatency, httpLatency int64
		err := rows.Scan(&r.IP, &r.Port, &r.CountryCode, &r.ISP, &tcpLatency, &r.TCPLossRate, &httpLatency, &r.DownloadSpeed, &r.Score)
		if err != nil {
			return nil, err
		}
		r.TCPLatencyAvg = time.Duration(tcpLatency) * time.Millisecond
		r.HTTPLatencyAvg = time.Duration(httpLatency) * time.Millisecond
		results = append(results, r)
	}
	return results, nil
}

// TrimToMaxSize 按综合评分末位淘汰，将数据库记录数控制在 maxSize 以内
// 删除规则: 评分最低的记录优先删除，评分相同时删除更新时间最早的
// 返回被删除的记录数
func (db *DB) TrimToMaxSize(maxSize int) (int64, error) {
	if maxSize <= 0 {
		return 0, nil
	}

	var currentCount int64
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM ip_results").Scan(&currentCount); err != nil {
		return 0, err
	}

	if currentCount <= int64(maxSize) {
		return 0, nil
	}

	// 需要淘汰的数量
	excess := currentCount - int64(maxSize)

	// 删除评分最低的记录（评分相同时删除更新时间最早的）
	result, err := db.conn.Exec(`
		DELETE FROM ip_results 
		WHERE rowid IN (
			SELECT rowid FROM ip_results 
			ORDER BY score ASC, updated_at ASC 
			LIMIT ?
		)`, excess)
	if err != nil {
		return 0, fmt.Errorf("末位淘汰失败: %w", err)
	}

	deleted, _ := result.RowsAffected()
	return deleted, nil
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	return db.conn.Close()
}
