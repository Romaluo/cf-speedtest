package updater

import "time"

// State 更新流程状态机
type State string

const (
	StateIdle        State = "idle"        // 空闲,无更新任务
	StateChecking    State = "checking"    // 正在检查版本
	StateDownloading State = "downloading" // 正在下载更新包
	StateVerifying   State = "verifying"   // 正在校验 sha256
	StateExtracting  State = "extracting"  // 正在解压
	StateInstalling  State = "installing"  // 正在替换二进制
	StateRestarting  State = "restarting"   // 正在重启进程
	StateFailed      State = "failed"      // 更新失败(已停止)
	StateDone        State = "done"        // 更新完成(短暂状态,随后进程重启)
)

// PlatformAsset 单个平台(GOOS/GOARCH)的下载资产信息
type PlatformAsset struct {
	URL    string `json:"url"`    // 下载 URL(HTTPS)
	Size   int64  `json:"size"`    // 文件字节数
	SHA256 string `json:"sha256"`  // sha256 校验和(小写十六进制)
}

// VersionManifest version.json 的结构
type VersionManifest struct {
	Version             string                   `json:"version"`               // 最新版本号(语义化,如 "1.2.0")
	ReleaseNotes        string                   `json:"release_notes"`         // 更新说明(支持 markdown)
	PublishedAt         time.Time                `json:"published_at"`          // 发布时间(ISO 8601)
	MinRequiredVersion string                   `json:"min_required_version"`  // 最低兼容版本(低于此版本不允许更新)
	Assets              map[string]PlatformAsset `json:"assets"`                // 按 "GOOS/GOARCH" 索引,如 "linux/amd64"
}

// StatusResponse GET /api/update/status 的响应
type StatusResponse struct {
	CurrentVersion      string    `json:"current_version"`                 // 当前运行的版本号
	LatestVersion       string    `json:"latest_version"`                  // version.json 中的最新版本号
	HasUpdate           bool      `json:"has_update"`                      // 是否有新版本可更新
	ReleaseNotes        string    `json:"release_notes"`                   // 更新说明(markdown)
	PublishedAt         time.Time `json:"published_at"`                    // 最新版本的发布时间
	UpdateSize          int64     `json:"update_size"`                     // 更新包字节数(当前平台)
	LastCheckAt         time.Time `json:"last_check_at"`                   // 最近一次检查时间
	LastCheckError      string    `json:"last_check_error"`                // 最近一次检查的错误信息(空=无错误)
	State               State     `json:"state"`                           // 当前更新流程状态
	MinRequiredVersion  string    `json:"min_required_version,omitempty"`  // 最低兼容版本(用于前端降级提示)
}

// ProgressEvent SSE 推送的进度事件
type ProgressEvent struct {
	State   State   `json:"state"`             // 当前状态
	Percent float64 `json:"percent"`          // 0-100,当前步骤进度
	Speed   string  `json:"speed,omitempty"`   // 下载速度(仅 StateDownloading)
	ETA     string  `json:"eta,omitempty"`     // 预计剩余时间(仅 StateDownloading)
	Message string  `json:"message,omitempty"` // 当前步骤说明
	Error   string  `json:"error,omitempty"`   // 错误信息(仅 StateFailed)
}
