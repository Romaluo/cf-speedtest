package pusher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cf-speedtest/config"
)

// WxPusher 微信推送器（基于 https://wxpusher.zjiecode.com）
//
// 推送 API: POST https://wxpusher.zjiecode.com/api/send/message
// 鉴权: 通过 appToken 字段在请求体中传递
// 接收方: topicIds（话题订阅）或 uids（指定用户），至少配置其一
type WxPusher struct {
	cfg    *config.Config
	client *http.Client
}

// NewWxPusher 创建 WxPusher 推送器
func NewWxPusher(cfg *config.Config) *WxPusher {
	return &WxPusher{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// wxPusherMessage WxPusher 推送消息请求体
type wxPusherMessage struct {
	AppToken   string   `json:"appToken"`
	Content    string   `json:"content"`
	Summary    string   `json:"summary,omitempty"`
	ContentType int     `json:"contentType"` // 1=text, 2=html, 3=markdown
	TopicIDs   []int    `json:"topicIds,omitempty"`
	UIDs       []string `json:"uids,omitempty"`
	URL        string   `json:"url,omitempty"`
}

// wxPusherResponse WxPusher 推送响应
type wxPusherResponse struct {
	Code      int    `json:"code"` // 1000 表示成功
	Msg       string `json:"msg"`
	Success   bool   `json:"success"`
	Data      []struct {
		Code     int    `json:"code"`
		Msg      string `json:"msg"`
		Success  bool   `json:"success"`
		Status   int    `json:"status"`
		TopicID  int    `json:"topicId"`
		UID      string `json:"uid"`
	} `json:"data"`
}

// Send 发送微信通知
// title 作为消息摘要（summary，限制 20 字符以内），content 作为消息正文
// contentType: 1=text, 2=html, 3=markdown
func (w *WxPusher) Send(title, content string, contentType int) error {
	if !w.cfg.WxPusherEnable {
		return nil // 未启用，直接返回
	}
	if w.cfg.WxPusherAppToken == "" {
		return fmt.Errorf("WxPusher 未配置 app_token")
	}
	if len(w.cfg.WxPusherTopicIDs) == 0 && len(w.cfg.WxPusherUIDs) == 0 {
		return fmt.Errorf("WxPusher 未配置接收方（topic_ids 或 uids 至少需配置一项）")
	}
	if contentType <= 0 {
		contentType = 1 // 默认文本
	}

	// summary 限制 20 字符
	summary := title
	if len([]rune(summary)) > 20 {
		summary = string([]rune(summary)[:20])
	}

	msg := wxPusherMessage{
		AppToken:    w.cfg.WxPusherAppToken,
		Content:     content,
		Summary:     summary,
		ContentType: contentType,
		TopicIDs:    w.cfg.WxPusherTopicIDs,
		UIDs:        w.cfg.WxPusherUIDs,
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal wxpusher message: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://wxpusher.zjiecode.com/api/send/message",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("wxpusher request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("wxpusher HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result wxPusherResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parse wxpusher response: %w", err)
	}

	// code=1000 表示请求成功，但单条消息可能仍有失败
	if result.Code != 1000 {
		return fmt.Errorf("wxpusher 失败: code=%d msg=%s", result.Code, result.Msg)
	}

	// 检查每条消息是否成功
	var failed []string
	for _, d := range result.Data {
		if !d.Success {
			failed = append(failed, fmt.Sprintf("topic=%d/uid=%s: %s", d.TopicID, d.UID, d.Msg))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("wxpusher 部分失败: %s", strings.Join(failed, "; "))
	}

	return nil
}

// NotifyJobComplete 任务完成时发送通知（便捷方法）
// jobType: "benchmark" / "push"
// success: 是否成功
// message: 完成消息（含统计信息）
func (w *WxPusher) NotifyJobComplete(jobType string, success bool, message string) error {
	if !w.cfg.WxPusherEnable {
		return nil
	}

	status := "✅ 成功"
	if !success {
		status = "❌ 失败"
	}

	title := fmt.Sprintf("CF优选-%s %s", jobTypeLabel(jobType), status)
	// markdown 格式通知
	content := fmt.Sprintf("## CF优选任务通知\n\n"+
		"- **任务类型**: %s\n"+
		"- **状态**: %s\n"+
		"- **时间**: %s\n"+
		"- **详情**: %s\n",
		jobTypeLabel(jobType), status,
		time.Now().Format("2006-01-02 15:04:05"),
		message)

	return w.Send(title, content, 3) // 3=markdown
}

// jobTypeLabel 任务类型中文标签
func jobTypeLabel(t string) string {
	switch t {
	case "benchmark":
		return "测速"
	case "push":
		return "推送"
	default:
		return t
	}
}
