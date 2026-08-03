package pusher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"cf-speedtest/config"
	"cf-speedtest/model"
)

// CloudflarePusher Cloudflare DNS 推送器
type CloudflarePusher struct {
	cfg    *config.Config
	client *http.Client
}

// NewCloudflarePusher 创建 Cloudflare 推送器
func NewCloudflarePusher(cfg *config.Config) *CloudflarePusher {
	return &CloudflarePusher{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// dnsRecord Cloudflare DNS 记录
type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// PushResult 推送结果统计
type PushResult struct {
	Created  int      `json:"created"`
	Updated  int      `json:"updated"`
	Deleted  int      `json:"deleted"`
	Retained int      `json:"retained"`
	IPs      []string `json:"ips"`
	Errors   []string `json:"errors,omitempty"`
}

// Push 将多个 IP 推送到 Cloudflare DNS（多IP模式）
// 逻辑: 1. 获取现有所有 A 记录 2. 通过 Batch API 单次提交删除+创建操作
// P1-5: 优先使用 Batch API（POST /dns_records/batch），失败时降级为单条操作
func (p *CloudflarePusher) Push(results []model.IPResult) error {
	if p.cfg.CFAPIKey == "" || p.cfg.CFZoneID == "" || p.cfg.CFDNSName == "" {
		return fmt.Errorf("Cloudflare 配置不完整: cf_api_key, cf_zone_id, cf_dns_name 均不能为空")
	}

	pushCount := p.cfg.CFPushCount
	if pushCount <= 0 {
		return nil // 0 表示不推送
	}
	if pushCount > len(results) {
		pushCount = len(results)
	}

	// 收集需要推送的 IP 列表（去重）
	newIPs := make([]string, 0, pushCount)
	seen := make(map[string]bool)
	for i := 0; i < pushCount && i < len(results); i++ {
		ip := results[i].IP
		if !seen[ip] {
			seen[ip] = true
			newIPs = append(newIPs, ip)
		}
	}

	if len(newIPs) == 0 {
		return fmt.Errorf("没有可推送的 IP")
	}

	fmt.Printf("[INFO] 准备将 %d 个 IP 推送到 Cloudflare DNS 记录 %s\n", len(newIPs), p.cfg.CFDNSName)

	// 1. 获取现有所有 A 记录
	existingRecords, err := p.getAllDNSRecords()
	if err != nil {
		fmt.Printf("[WARN] 查询现有 DNS 记录失败: %v，将直接创建新记录\n", err)
		existingRecords = []dnsRecord{}
	}

	// 2. 对比并执行增删
	result := PushResult{IPs: newIPs}
	existingMap := make(map[string]dnsRecord) // IP -> record
	for _, r := range existingRecords {
		existingMap[r.Content] = r
	}

	// 3. 尝试使用 Batch API 单次提交（P1-5）
	batchRes, batchErr := p.batchUpdateDNS(newIPs, existingMap)
	if batchErr == nil {
		result.Created = batchRes.Created
		result.Deleted = batchRes.Deleted
		result.Retained = batchRes.Retained
		result.Errors = batchRes.Errors
		fmt.Printf("[INFO] Cloudflare DNS Batch API 推送完成: 新建 %d, 保留 %d, 删除 %d\n",
			result.Created, result.Retained, result.Deleted)
		if len(result.Errors) > 0 {
			return fmt.Errorf("Batch 推送部分失败: %v", result.Errors)
		}
		return nil
	}

	// 降级：Batch API 失败时使用单条操作
	fmt.Printf("[WARN] Batch API 失败: %v，降级为单条操作\n", batchErr)

	newIPSet := make(map[string]bool, len(newIPs))
	for _, ip := range newIPs {
		newIPSet[ip] = true
	}
	for ip, record := range existingMap {
		if !newIPSet[ip] {
			if err := p.deleteDNSRecord(record.ID); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("删除旧记录 %s 失败: %v", ip, err))
			} else {
				result.Deleted++
				fmt.Printf("[INFO] 已删除旧 DNS 记录: %s (%s)\n", ip, record.ID)
			}
		} else {
			result.Retained++ // 已存在且仍需保留
		}
	}

	// 4. 为新IP创建记录（已存在的跳过）
	for _, ip := range newIPs {
		if _, exists := existingMap[ip]; exists {
			continue
		}
		if err := p.createDNSRecord(ip); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("创建记录 %s 失败: %v", ip, err))
		} else {
			result.Created++
			fmt.Printf("[INFO] 已创建 DNS A 记录: %s → %s\n", p.cfg.CFDNSName, ip)
		}
	}

	fmt.Printf("[INFO] Cloudflare DNS 推送完成: 新建 %d, 保留 %d, 删除 %d, 当前共 %d 条A记录\n",
		result.Created, result.Retained, result.Deleted, len(newIPs))

	if len(result.Errors) > 0 {
		return fmt.Errorf("推送部分失败: %v", result.Errors)
	}

	return nil
}

// batchUpdateDNS 使用 Cloudflare Batch API 单次提交删除+创建操作
// 端点: POST /zones/{zone_id}/dns_records/batch
// Cloudflare 批量 API 限制: 单次最多 200 个操作（删除+创建合计）
func (p *CloudflarePusher) batchUpdateDNS(newIPs []string, existingMap map[string]dnsRecord) (*PushResult, error) {
	ttl := p.cfg.CFDNSTTL
	if ttl <= 0 {
		ttl = 300
	}
	proxied := p.cfg.CFOptions == "proxied=true"

	newIPSet := make(map[string]bool, len(newIPs))
	for _, ip := range newIPs {
		newIPSet[ip] = true
	}

	// 构造批量请求体
	type postRecord struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		TTL     int    `json:"ttl"`
		Proxied bool   `json:"proxied"`
	}
	type batchPayload struct {
		Posts   []postRecord `json:"posts"`
		Deletes []string     `json:"deletes"`
	}

	payload := batchPayload{}
	result := &PushResult{}

	// 1. 收集需要删除的记录 ID（不在新列表中的旧记录）
	for ip, record := range existingMap {
		if !newIPSet[ip] {
			payload.Deletes = append(payload.Deletes, record.ID)
		} else {
			result.Retained++
		}
	}

	// 2. 收集需要创建的记录（新列表中不存在的IP）
	for _, ip := range newIPs {
		if _, exists := existingMap[ip]; exists {
			continue
		}
		payload.Posts = append(payload.Posts, postRecord{
			Type:    "A",
			Name:    p.cfg.CFDNSName,
			Content: ip,
			TTL:     ttl,
			Proxied: proxied,
		})
	}

	// 如果没有操作需要执行，直接返回
	if len(payload.Posts) == 0 && len(payload.Deletes) == 0 {
		return result, nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal batch payload: %w", err)
	}

	batchURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/batch", p.cfg.CFZoneID)
	req, err := http.NewRequest("POST", batchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.CFAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("batch HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析批量响应
	// 实际 Cloudflare Batch API 响应格式：
	//   成功: {"result":{"posts":[{...完整DNS记录...}],"deletes":[{...}]}, "success":true, "errors":[]}
	//   失败: {"result":null, "success":false, "errors":[{"code":9207,"message":"..."}]}
	// 注意: posts[]/deletes[] 内是 DNS 记录对象（无 success 字段），
	//       整体成败通过顶层 success + errors 判断，而非每条记录内的 success 字段。
	var batchResp struct {
		Success bool `json:"success"`
		Result  struct {
			Posts   []json.RawMessage `json:"posts"`
			Deletes []json.RawMessage `json:"deletes"`
		} `json:"result"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &batchResp); err != nil {
		return nil, fmt.Errorf("parse batch response: %w", err)
	}

	// 顶层 success=false 表示整个 batch 请求失败
	if !batchResp.Success {
		msg := "unknown batch error"
		if len(batchResp.Errors) > 0 {
			err := batchResp.Errors[0]
			if err.Message != "" {
				msg = err.Message
			} else {
				msg = fmt.Sprintf("code=%d", err.Code)
			}
		}
		return result, fmt.Errorf("batch failed: %s", msg)
	}

	// 顶层 success=true 时，posts/deletes 数组长度即成功操作数
	// (数组元素为创建/删除的记录对象详情)
	result.Created = len(batchResp.Result.Posts)
	result.Deleted = len(batchResp.Result.Deletes)

	return result, nil
}

// getAllDNSRecords 获取指定名称的所有 A 记录
func (p *CloudflarePusher) getAllDNSRecords() ([]dnsRecord, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=A",
		p.cfg.CFZoneID, p.cfg.CFDNSName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.CFAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Success bool        `json:"success"`
		Result  []dnsRecord `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if !result.Success {
		if len(result.Errors) > 0 {
			return nil, fmt.Errorf("%s", result.Errors[0].Message)
		}
		return nil, fmt.Errorf("查询 DNS 记录失败")
	}

	return result.Result, nil
}

// createDNSRecord 创建新的 A 记录
func (p *CloudflarePusher) createDNSRecord(ip string) error {
	ttl := p.cfg.CFDNSTTL
	if ttl <= 0 {
		ttl = 300
	}

	proxied := p.cfg.CFOptions == "proxied=true"

	payload := map[string]interface{}{
		"type":    "A",
		"name":    p.cfg.CFDNSName,
		"content": ip,
		"ttl":     ttl,
		"proxied": proxied,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.cfg.CFZoneID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.CFAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// deleteDNSRecord 删除指定的 DNS 记录
func (p *CloudflarePusher) deleteDNSRecord(recordID string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s",
		p.cfg.CFZoneID, recordID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.CFAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// updateDNSRecord 更新现有 DNS 记录（保留用于单条更新场景）
func (p *CloudflarePusher) updateDNSRecord(recordID, newIP string) error {
	ttl := p.cfg.CFDNSTTL
	if ttl <= 0 {
		ttl = 300
	}

	proxied := p.cfg.CFOptions == "proxied=true"

	payload := map[string]interface{}{
		"type":    "A",
		"name":    p.cfg.CFDNSName,
		"content": newIP,
		"ttl":     ttl,
		"proxied": proxied,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s",
		p.cfg.CFZoneID, recordID)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.CFAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
