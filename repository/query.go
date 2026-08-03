package repository

import (
	"fmt"
	"strings"
	"time"

	"cf-speedtest/model"
)

// ResultFilter 结果查询过滤条件
type ResultFilter struct {
	Limit    int     // 返回条数上限
	Offset   int     // 偏移量
	ISP      string  // 按运营商精确匹配（为空则不过滤）
	Country  string  // 按国家代码精确匹配（为空则不过滤）
	MinPort  int     // 最小端口（0 不过滤）
	MaxPort  int     // 最大端口（0 不过滤）
	MinScore float64 // 最低评分（0 不过滤）
	Sort     string  // 排序字段: score(默认)/latency/loss_rate/http_latency/bandwidth/updated_at
	Order    string  // asc/desc（默认 desc）
}

// ResultRow 带更新时间的查询结果行（用于 Web 展示）
type ResultRow struct {
	model.IPResult
	UpdatedAt time.Time `json:"updated_at"`
}

// ListResults 按过滤条件分页查询结果
func (db *DB) ListResults(f ResultFilter) ([]ResultRow, int, error) {
	var (
		conditions []string
		args       []interface{}
	)

	if f.ISP != "" {
		conditions = append(conditions, "isp = ?")
		args = append(args, f.ISP)
	}
	if f.Country != "" {
		conditions = append(conditions, "country_code = ?")
		args = append(args, strings.ToUpper(f.Country))
	}
	if f.MinPort > 0 {
		conditions = append(conditions, "port >= ?")
		args = append(args, f.MinPort)
	}
	if f.MaxPort > 0 {
		conditions = append(conditions, "port <= ?")
		args = append(args, f.MaxPort)
	}
	if f.MinScore > 0 {
		conditions = append(conditions, "score >= ?")
		args = append(args, f.MinScore)
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// 统计总数
	var total int
	countSQL := "SELECT COUNT(*) FROM ip_results" + where
	if err := db.conn.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 排序
	sortCol, order := normalizeSort(f.Sort, f.Order)

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 5000 {
		limit = 5000
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	querySQL := fmt.Sprintf(`
		SELECT ip, port, country_code, isp, tcp_latency_avg, tcp_loss_rate,
		       http_latency_avg, download_speed, score, updated_at
		FROM ip_results%s
		ORDER BY %s %s
		LIMIT ? OFFSET ?`, where, sortCol, order)

	queryArgs := append(args, limit, offset)
	rows, err := db.conn.Query(querySQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var results []ResultRow
	for rows.Next() {
		var r ResultRow
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
			&r.UpdatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		r.TCPLatencyAvg = time.Duration(tcpLatency) * time.Millisecond
		r.HTTPLatencyAvg = time.Duration(httpLatency) * time.Millisecond
		results = append(results, r)
	}
	return results, total, nil
}

func normalizeSort(sort, order string) (string, string) {
	var sortCol string
	switch strings.ToLower(strings.TrimSpace(sort)) {
	case "latency":
		sortCol = "tcp_latency_avg"
	case "loss_rate":
		sortCol = "tcp_loss_rate"
	case "http_latency":
		sortCol = "http_latency_avg"
	case "bandwidth":
		sortCol = "download_speed"
	case "updated_at":
		sortCol = "updated_at"
	default:
		sortCol = "score"
	}
	if strings.ToLower(strings.TrimSpace(order)) == "asc" {
		order = "ASC"
	} else {
		order = "DESC"
	}
	return sortCol, order
}

// DeleteResult 删除单条记录
func (db *DB) DeleteResult(ip string, port int) error {
	_, err := db.conn.Exec("DELETE FROM ip_results WHERE ip = ? AND port = ?", ip, port)
	return err
}

// DeleteAll 清空全部记录
func (db *DB) DeleteAll() (int64, error) {
	res, err := db.conn.Exec("DELETE FROM ip_results")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats 统计信息
type Stats struct {
	Total         int64        `json:"total"`
	ByISP         []GroupCount `json:"by_isp"`
	ByCountry     []GroupCount `json:"by_country"`
	ByPort        []GroupCount `json:"by_port"`
	AvgLatency    float64      `json:"avg_latency_ms"`
	AvgLossRate   float64      `json:"avg_loss_rate"`
	AvgBandwidth  float64      `json:"avg_bandwidth_mbps"`
	MaxBandwidth  float64      `json:"max_bandwidth_mbps"`
	MinLatency    float64      `json:"min_latency_ms"`
	LatestUpdated *time.Time   `json:"latest_updated_at"`
	TopScore      float64      `json:"top_score"`
}

// GroupCount 分组计数
type GroupCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// GetStats 获取数据库统计信息
func (db *DB) GetStats() (*Stats, error) {
	s := &Stats{}

	if err := db.conn.QueryRow("SELECT COUNT(*) FROM ip_results").Scan(&s.Total); err != nil {
		return nil, err
	}

	if s.Total == 0 {
		return s, nil
	}

	// 聚合指标
	err := db.conn.QueryRow(`
		SELECT 
			COALESCE(AVG(tcp_latency_avg), 0),
			COALESCE(AVG(tcp_loss_rate), 0),
			COALESCE(AVG(download_speed), 0),
			COALESCE(MAX(download_speed), 0),
			COALESCE(MIN(tcp_latency_avg), 0),
			COALESCE(MAX(score), 0)
		FROM ip_results`).Scan(
		&s.AvgLatency, &s.AvgLossRate, &s.AvgBandwidth, &s.MaxBandwidth,
		&s.MinLatency, &s.TopScore)
	if err != nil {
		return nil, err
	}

	// 单独获取 MAX(updated_at) 避免类型扫描问题
	var latestStr string
	if err := db.conn.QueryRow("SELECT MAX(updated_at) FROM ip_results").Scan(&latestStr); err == nil && latestStr != "" {
		if t, perr := time.Parse("2006-01-02 15:04:05", latestStr); perr == nil {
			s.LatestUpdated = &t
		} else if t, perr := time.Parse(time.RFC3339, latestStr); perr == nil {
			s.LatestUpdated = &t
		}
	}

	s.ByISP, err = db.groupCount("isp")
	if err != nil {
		return nil, err
	}
	s.ByCountry, err = db.groupCount("country_code")
	if err != nil {
		return nil, err
	}
	s.ByPort, err = db.groupCount("port")
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (db *DB) groupCount(col string) ([]GroupCount, error) {
	rows, err := db.conn.Query(fmt.Sprintf(`
		SELECT COALESCE(%s, '') AS label, COUNT(*) AS cnt
		FROM ip_results
		GROUP BY %s
		ORDER BY cnt DESC`, col, col))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []GroupCount
	for rows.Next() {
		var g GroupCount
		if err := rows.Scan(&g.Label, &g.Count); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// DistinctISPs 返回所有出现过的运营商（去重）
func (db *DB) DistinctISPs() ([]string, error) {
	return db.distinctValues("isp")
}

// DistinctCountries 返回所有出现过的国家代码（去重）
func (db *DB) DistinctCountries() ([]string, error) {
	return db.distinctValues("country_code")
}

func (db *DB) distinctValues(col string) ([]string, error) {
	rows, err := db.conn.Query(fmt.Sprintf(`
		SELECT DISTINCT %s FROM ip_results WHERE %s IS NOT NULL AND %s != '' ORDER BY %s`,
		col, col, col, col))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}
