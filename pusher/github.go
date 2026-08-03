package pusher

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"cf-speedtest/config"
	"cf-speedtest/model"
	"cf-speedtest/scorer"
)

// GithubPusher GitHub 推送器
type GithubPusher struct {
	cfg    *config.Config
	client *http.Client
}

// NewGithubPusher 创建 GitHub 推送器
func NewGithubPusher(cfg *config.Config) *GithubPusher {
	return &GithubPusher{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Push 将前 N 个 IP 写入 IP.txt 并推送到 GitHub
func (g *GithubPusher) Push(results []model.IPResult) error {
	if g.cfg.GithubToken == "" || g.cfg.GithubRepo == "" {
		return fmt.Errorf("GitHub 配置不完整: github_token, github_repo 均不能为空")
	}

	pushCount := g.cfg.GithubPushCount
	if pushCount <= 0 {
		return nil // 0 表示不推送
	}
	if pushCount > len(results) {
		pushCount = len(results)
	}

	// 生成 IP.txt 内容
	content := g.generateIPText(results, pushCount)
	if content == "" {
		return fmt.Errorf("没有可推送的 IP 内容")
	}

	fmt.Printf("[INFO] 准备将前 %d 个 IP 推送到 GitHub %s/%s\n",
		pushCount, g.cfg.GithubRepo, g.cfg.GithubFilePath)

	// 获取现有文件的 SHA（用于更新）
	existingSHA, _ := g.getFileSHA()

	// 提交到 GitHub
	if err := g.createOrUpdateFile(content, existingSHA); err != nil {
		return fmt.Errorf("GitHub 推送失败: %w", err)
	}

	fmt.Printf("[INFO] 已成功推送到 GitHub: %s/%s (分支: %s)\n",
		g.cfg.GithubRepo, g.cfg.GithubFilePath, g.cfg.GithubBranch)
	return nil
}

// generateIPText 生成 IP.txt 内容
// 格式: IP:端口#国家代码 评分等级 带宽 Mbps 延迟 ms（与本地 IP.txt 一致，无表头）
func (g *GithubPusher) generateIPText(results []model.IPResult, count int) string {
	var buf bytes.Buffer
	for i := 0; i < count && i < len(results); i++ {
		r := results[i]
		countryCode := r.CountryCode
		if countryCode == "" {
			countryCode = "-"
		}
		latencyMs := float64(r.TCPLatencyAvg.Milliseconds())
		bandwidthMbps := r.DownloadSpeed * 8 // MB/s → Mbps
		fmt.Fprintf(&buf, "%s:%d#%s %s %.2f Mbps %.2f ms\n",
			r.IP, r.Port, countryCode, scorer.GetQualityGrade(r.Score), bandwidthMbps, latencyMs)
	}
	return buf.String()
}

// getFileSHA 获取 GitHub 仓库中文件的 SHA
func (g *GithubPusher) getFileSHA() (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s",
		g.cfg.GithubRepo, g.cfg.GithubFilePath, g.cfg.GithubBranch)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.GithubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil // 文件不存在，需要创建
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result struct {
		SHA string `json:"sha"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	return result.SHA, nil
}

// createOrUpdateFile 创建或更新 GitHub 文件
func (g *GithubPusher) createOrUpdateFile(content, existingSHA string) error {
	branch := g.cfg.GithubBranch
	if branch == "" {
		branch = "main"
	}

	encodedContent := base64.StdEncoding.EncodeToString([]byte(content))

	commitMsg := fmt.Sprintf("chore: update %s - %s",
		g.cfg.GithubFilePath, time.Now().Format("2006-01-02 15:04:05"))

	payload := map[string]interface{}{
		"message": commitMsg,
		"content": encodedContent,
		"branch":  branch,
	}

	if existingSHA != "" {
		payload["sha"] = existingSHA
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s",
		g.cfg.GithubRepo, g.cfg.GithubFilePath)

	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.GithubToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API 返回错误: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
