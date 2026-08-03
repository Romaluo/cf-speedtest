package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"cf-speedtest/model"
)

// ipapiIsResponse ipapi.is API 响应结构（仅提取风险相关字段）
// 文档: https://api.ipapi.is/?q=IP
type ipapiIsResponse struct {
	IP string `json:"ip"`
	// 顶层 risk_score（新版字段）
	RiskScore int `json:"risk_score"`
	// is_bogon / is_datacenter 等辅助判断字段
	IsBogon      bool `json:"is_bogon"`
	IsDatacenter bool `json:"is_datacenter"`
	IsTor        bool `json:"is_tor"`
	IsProxy      bool `json:"is_proxy"`
	IsVpn        bool `json:"is_vpn"`
	IsAbuser     bool `json:"is_abuser"`
	IsCrawler    bool `json:"is_crawler"`
	// 兼容旧版：嵌套在 company/security 对象中的 risk_score
	Company struct {
		Network    string `json:"network"`
		Abuser     string `json:"abuser"`
		Datacenter struct {
			Datacenter string `json:"datacenter"`
			Domain     string `json:"domain"`
			Network    string `json:"network"`
		} `json:"datacenter"`
	} `json:"company"`
}

// RiskCheckResult 单个 IP 的风险检查结果
type RiskCheckResult struct {
	IP        string
	Safe      bool   // 是否安全（risk_score <= threshold 且非显式风险标记）
	RiskScore int    // 风险分数 0-100
	Reason    string // 不安全原因
}

// FilterByRisk 在 DNS 推送前过滤高风险 IP
// 并发查询每个 IP 的风险分数，risk_score > threshold 的 IP 被过滤
// 返回过滤后的安全 IP 列表（保持原排序顺序）
func FilterByRisk(results []model.IPResult, threshold int, timeout time.Duration, concurrency int) ([]model.IPResult, []RiskCheckResult) {
	if len(results) == 0 {
		return results, nil
	}
	if threshold <= 0 {
		// 阈值<=0 视为禁用过滤
		return results, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 10
	}

	// 去重查询：同一 IP 只查一次
	uniqueIPs := make(map[string]struct{})
	for _, r := range results {
		uniqueIPs[r.IP] = struct{}{}
	}

	ipList := make([]string, 0, len(uniqueIPs))
	for ip := range uniqueIPs {
		ipList = append(ipList, ip)
	}

	// 并发查询风险分数
	type queryResult struct {
		ip  string
		r   *RiskCheckResult
		err error
	}
	resultsCh := make(chan queryResult, len(ipList))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, ip := range ipList {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rr, err := checkIPRisk(ip, threshold, timeout)
			resultsCh <- queryResult{ip: ip, r: rr, err: err}
		}(ip)
	}
	wg.Wait()
	close(resultsCh)

	// 收集查询结果
	riskMap := make(map[string]*RiskCheckResult, len(ipList))
	var allChecks []RiskCheckResult
	for q := range resultsCh {
		if q.err != nil || q.r == nil {
			// 查询失败时默认放行（避免外部 API 故障影响主流程）
			rr := &RiskCheckResult{IP: q.ip, Safe: true, RiskScore: -1, Reason: "查询失败，默认放行"}
			riskMap[q.ip] = rr
			allChecks = append(allChecks, *rr)
			continue
		}
		riskMap[q.ip] = q.r
		allChecks = append(allChecks, *q.r)
	}

	// 过滤高风险 IP（保持原顺序）
	safe := make([]model.IPResult, 0, len(results))
	for _, r := range results {
		if rr, ok := riskMap[r.IP]; ok && !rr.Safe {
			continue // 跳过高风险 IP
		}
		safe = append(safe, r)
	}

	// 按 RiskScore 升序排序查询结果（便于调试）
	sort.Slice(allChecks, func(i, j int) bool {
		return allChecks[i].RiskScore < allChecks[j].RiskScore
	})

	return safe, allChecks
}

// checkIPRisk 查询单个 IP 的风险分数
func checkIPRisk(ip string, threshold int, timeout time.Duration) (*RiskCheckResult, error) {
	url := fmt.Sprintf("https://api.ipapi.is/?q=%s", ip)
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// P2-10: 直连模式 — 显式禁用代理
			Proxy: nil,
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cf-speedtest/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("risk query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("risk query HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp ipapiIsResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parse risk response: %w", err)
	}

	score := apiResp.RiskScore
	reason := ""

	// 显式风险标记直接判不安全（即使分数未超阈值）
	unsafe := false
	if apiResp.IsTor {
		unsafe = true
		reason = "Tor 出口节点"
	} else if apiResp.IsProxy {
		unsafe = true
		reason = "代理服务器"
	} else if apiResp.IsVpn {
		unsafe = true
		reason = "VPN 节点"
	} else if apiResp.IsAbuser {
		unsafe = true
		reason = "滥用历史"
	} else if apiResp.IsBogon {
		unsafe = true
		reason = "Bogon 地址"
	}

	// 分数超过阈值也判不安全
	if !unsafe && score > threshold {
		unsafe = true
		reason = fmt.Sprintf("risk_score=%d > %d", score, threshold)
	}

	return &RiskCheckResult{
		IP:        ip,
		Safe:      !unsafe,
		RiskScore: score,
		Reason:    reason,
	}, nil
}
