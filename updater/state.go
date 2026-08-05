package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// updateStateFilename update_state.json 文件名(与可执行文件同目录)
const updateStateFilename = "update_state.json"

// UpdateState 更新状态文件结构
// 由 Installer 在 Windows 平台替换二进制时写入,新进程启动时读取并清理
// 主要用途:
//   - 记录需要在新进程启动后清理的 .old 文件列表(Windows 替换二进制策略)
//   - 记录上次更新的版本号、时间、是否成功,用于启动日志输出
type UpdateState struct {
	OldFiles    []string  `json:"old_files"`    // 待清理的旧文件路径列表(如 exe+".old")
	LastVersion string    `json:"last_version"` // 上次更新到的版本号
	UpdatedAt   time.Time `json:"updated_at"`   // 状态写入时间
	Success     bool      `json:"success"`      // 是否成功完成替换(用于新进程启动时记录日志)
}

// SaveState 将 UpdateState 写入指定路径的 JSON 文件
// 写入采用 "临时文件 + rename" 模式,确保原子性(避免写入中途崩溃导致文件损坏)
func SaveState(path string, state *UpdateState) error {
	if state == nil {
		return fmt.Errorf("state 不能为 nil")
	}

	// 确保父目录存在
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("创建状态文件目录失败: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}

	// 原子写入:先写入临时文件,再 rename 覆盖
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("写入临时状态文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("原子替换状态文件失败: %w", err)
	}
	return nil
}

// LoadState 从指定路径读取 UpdateState
// 文件不存在时返回 nil + nil(不视为错误,表示首次运行或已清理)
func LoadState(path string) (*UpdateState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取状态文件失败: %w", err)
	}
	var state UpdateState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析状态文件失败: %w", err)
	}
	return &state, nil
}

// DeleteState 删除状态文件(清理完成后调用)
// 文件不存在时返回 nil(幂等)
func DeleteState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除状态文件失败: %w", err)
	}
	return nil
}

// nowTime 返回当前时间(包装 time.Now,便于测试时 mock)
func nowTime() time.Time {
	return time.Now()
}

// DefaultStatePath 返回默认的 update_state.json 路径(与可执行文件同目录)
// 如果无法获取可执行文件路径(罕见),退化到当前工作目录
func DefaultStatePath() string {
	exe, err := os.Executable()
	if err != nil {
		return updateStateFilename
	}
	if realPath, err := filepath.EvalSymlinks(exe); err == nil {
		exe = realPath
	}
	return filepath.Join(filepath.Dir(exe), updateStateFilename)
}

// CleanupOnStartup 在新进程启动时清理上次更新遗留的临时文件
// 主要用途(Windows 策略下,替换二进制时旧 exe 被重命名为 .old,需新进程启动后删除):
//   - 删除 update_state.json 中记录的所有 old_files
//   - 日志记录上次更新结果(success=true 时记录"上次更新到 vX.Y.Z 成功",否则记录警告)
//   - 清理完成后删除 update_state.json(避免重复清理)
//
// 调用时机:main() 早期,在 logger 初始化之后
func CleanupOnStartup(logger interface {
	Info(category, format string, args ...interface{})
	Warn(category, format string, args ...interface{})
}) {
	statePath := DefaultStatePath()
	state, err := LoadState(statePath)
	if err != nil {
		// 状态文件损坏(可能是上次写入中断):记录警告并尝试删除
		if logger != nil {
			logger.Warn("UPDATE", "读取 update_state.json 失败: %v", err)
		}
		_ = os.Remove(statePath)
		return
	}
	if state == nil {
		// 文件不存在(首次运行或已清理),无需处理
		return
	}

	// 清理 old_files 列表中的所有遗留文件(主要是 Windows 的 .old 二进制)
	for _, f := range state.OldFiles {
		if err := os.Remove(f); err != nil {
			if !os.IsNotExist(err) && logger != nil {
				logger.Warn("UPDATE", "清理旧文件失败 %s: %v", f, err)
			}
		} else if logger != nil {
			logger.Info("UPDATE", "已清理上次更新的旧文件: %s", f)
		}
	}

	// 日志记录上次更新结果
	if logger != nil {
		if state.Success {
			if state.LastVersion != "" {
				logger.Info("UPDATE", "上次更新到 v%s 成功(更新时间: %s)",
					state.LastVersion, state.UpdatedAt.Format("2006-01-02 15:04:05"))
			} else {
				logger.Info("UPDATE", "上次更新成功(更新时间: %s)",
					state.UpdatedAt.Format("2006-01-02 15:04:05"))
			}
		} else {
			logger.Warn("UPDATE", "上次更新未成功完成(更新时间: %s)",
				state.UpdatedAt.Format("2006-01-02 15:04:05"))
		}
	}

	// 清理后删除状态文件
	if err := DeleteState(statePath); err != nil && logger != nil {
		logger.Warn("UPDATE", "删除 update_state.json 失败: %v", err)
	}
}
