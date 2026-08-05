package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cf-speedtest/log"
)

// Installer 安装器:解压下载的更新包 + 平台分支替换二进制
//
// 流程:Extract(解压) → Install(替换) → 由 Manager 调用 Restarter 重启
// 失败回滚:替换前备份当前二进制到 .bak,任意失败 → 恢复 .bak + 清理临时文件
type Installer struct {
	logger *log.Logger // 可为 nil
}

// NewInstaller 创建安装器
func NewInstaller(logger *log.Logger) *Installer {
	return &Installer{logger: logger}
}

// Extract 解压已下载的更新包到临时目录
// 根据平台选择解压方式:Linux tar.gz / Windows zip
// 解压目标:{tempDir}/cf-speedtest-{version}/
// 返回解压后的目录路径,供后续 Install() 使用
func (in *Installer) Extract(archivePath, version string) (string, error) {
	extractStart := time.Now()

	// 解压到与下载文件同目录下的 cf-speedtest-{version}/ 子目录
	tempDir := filepath.Dir(archivePath)
	extractDir := filepath.Join(tempDir, fmt.Sprintf("cf-speedtest-%s", version))

	// 获取压缩包大小用于日志
	var archiveSize int64
	if info, err := os.Stat(archivePath); err == nil {
		archiveSize = info.Size()
	}

	if in.logger != nil {
		in.logger.Info("UPDATE", "开始解压: %s (%s, %d 字节) → %s",
			filepath.Base(archivePath), runtime.GOOS, archiveSize, extractDir)
	}

	// 如果目标目录已存在(上次解压残留),先清理避免脏数据
	if err := os.RemoveAll(extractDir); err != nil {
		return "", fmt.Errorf("清理旧解压目录失败: %w", err)
	}
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return "", fmt.Errorf("创建解压目录失败: %w", err)
	}

	var err error
	switch runtime.GOOS {
	case "windows":
		err = extractZip(archivePath, extractDir)
	default:
		err = extractTarGz(archivePath, extractDir)
	}
	if err != nil {
		// 解压失败:清理已创建的目录,避免残留
		_ = os.RemoveAll(extractDir)
		if in.logger != nil {
			in.logger.Error("UPDATE", "解压失败: %v", err)
		}
		return "", fmt.Errorf("解压失败: %w", err)
	}

	// 统计解压后的文件数(用于日志校验)
	fileCount := countFiles(extractDir)

	if in.logger != nil {
		in.logger.Info("UPDATE", "解压完成: %d 个文件 (耗时 %s)",
			fileCount, time.Since(extractStart).Round(time.Millisecond))
	}
	return extractDir, nil
}

// countFiles 统计目录下的文件总数(递归,含子目录)
func countFiles(dir string) int {
	count := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		count++
		return nil
	})
	return count
}

// extractTarGz 解压 tar.gz 文件(Linux/macOS)
func extractTarGz(srcPath, destDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("打开 tar.gz 失败: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("初始化 gzip 解压器失败: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 条目失败: %w", err)
		}

		// 安全检查:防止路径穿越(zip slip 攻击)
		target := filepath.Join(destDir, hdr.Name)
		if !isPathSafe(destDir, target) {
			return fmt.Errorf("检测到路径穿越攻击: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0777); err != nil {
				return fmt.Errorf("创建目录失败 %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("创建父目录失败 %s: %w", filepath.Dir(target), err)
			}
			// 校验写入字节数与 tar 头声明一致(防止解压中途被截断)
			written, err := writeFileWithCount(target, tr, os.FileMode(hdr.Mode)&0777)
			if err != nil {
				return fmt.Errorf("写入文件失败 %s: %w", target, err)
			}
			if hdr.Size > 0 && written != hdr.Size {
				return fmt.Errorf("文件 %s 大小不匹配: 写入 %d, 期望 %d(可能下载损坏)", target, written, hdr.Size)
			}
		case tar.TypeSymlink:
			// 出于安全考虑,跳过符号链接(更新包不应包含符号链接)
			continue
		default:
			// 跳过其他类型(fifo/device 等)
			continue
		}
	}
	return nil
}

// extractZip 解压 zip 文件(Windows)
func extractZip(srcPath, destDir string) error {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer zr.Close()

	for _, zf := range zr.File {
		// 安全检查:防止路径穿越
		target := filepath.Join(destDir, zf.Name)
		if !isPathSafe(destDir, target) {
			return fmt.Errorf("检测到路径穿越攻击: %s", zf.Name)
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("创建目录失败 %s: %w", target, err)
			}
			continue
		}

		// 确保父目录存在
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Errorf("创建父目录失败 %s: %w", filepath.Dir(target), err)
		}

		// 打开 zip 内文件
		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("打开 zip 条目失败 %s: %w", zf.Name, err)
		}
		err = writeFile(target, rc, zf.Mode())
		rc.Close()
		if err != nil {
			return fmt.Errorf("写入文件失败 %s: %w", target, err)
		}
	}
	return nil
}

// writeFile 写入文件(指定权限),完成后 fsync 确保落盘
func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	// 确保数据落盘(更新场景下数据完整性很重要)
	// Sync 失败也视为写入失败,避免数据未真正落盘的"假成功"
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync 失败: %w", err)
	}
	return nil
}

// writeFileWithCount 写入文件并返回写入字节数,完成后 fsync 确保落盘
// 用于解压时校验实际写入字节数与 tar 头声明的 size 是否一致
func writeFileWithCount(path string, r io.Reader, mode os.FileMode) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	// 确保数据落盘(更新场景下数据完整性很重要)
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("fsync 失败: %w", err)
	}
	return n, nil
}

// isPathSafe 检查 target 是否在 baseDir 内(防止路径穿越攻击)
// 同时防御 Unix (`../`) 和 Windows (`..\`) 风格的路径穿越
func isPathSafe(baseDir, target string) bool {
	// 都转为绝对路径后比较,确保 target 真的在 baseDir 子树内
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	// 标准化路径分隔符(Windows 下可能混用 / 和 \)
	absBase = filepath.Clean(absBase)
	absTarget = filepath.Clean(absTarget)

	// target 必须等于 baseDir 或在 baseDir/ 之下
	if absTarget == absBase {
		return true
	}
	// 统一用 Separator 后缀比较,避免前缀误匹配(如 /tmp/foo vs /tmp/foobar)
	expectedPrefix := absBase + string(filepath.Separator)
	if !strings.HasPrefix(absTarget, expectedPrefix) {
		return false
	}
	// 额外检查:相对路径不能包含 `..`(理论上 Clean 已处理,但双保险)
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// Install 替换运行中的二进制 + 资源文件
// 平台分支:
//   - Linux:写入 exe+".new" → 原子 os.Rename 覆盖运行中文件
//   - Windows:重命名当前 exe 为 .old → 写入新 exe → 标记启动后清理 .old
//
// 资源文件替换(可选,新版本未带则跳过):
//   - ip2region.xdb(数据库文件)
//   - config.yaml.example(示例配置,不覆盖用户 config.yaml)
//
// 失败回滚:替换前备份当前二进制到 .bak,失败时恢复
// Windows 的 .old 文件由新进程启动时通过 update_state.json 清理
func (in *Installer) Install(extractDir string) error {
	installStart := time.Now()

	// 定位解压目录中的新二进制文件
	newExePath, err := findNewBinary(extractDir)
	if err != nil {
		return err
	}
	if in.logger != nil {
		// 获取新二进制大小用于日志
		var newSize int64
		if info, err := os.Stat(newExePath); err == nil {
			newSize = info.Size()
		}
		in.logger.Info("UPDATE", "定位到新二进制: %s (%d 字节)", filepath.Base(newExePath), newSize)
	}

	// 当前运行中的可执行文件路径
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	// 解析符号链接(确保拿到真实路径,避免 .new 写到符号链接所在目录)
	if realPath, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = realPath
	}
	exeDir := filepath.Dir(exePath)

	if in.logger != nil {
		in.logger.Info("UPDATE", "开始替换二进制: %s → %s (平台: %s/%s)",
			filepath.Base(newExePath), exePath, runtime.GOOS, runtime.GOARCH)
	}

	// Step 1: 备份当前二进制(用于失败回滚)
	bakPath := exePath + ".bak"
	if err := copyFile(exePath, bakPath, 0755); err != nil {
		return fmt.Errorf("备份当前二进制失败: %w", err)
	}
	if in.logger != nil {
		in.logger.Info("UPDATE", "已备份当前二进制: %s", bakPath)
	}

	// Step 2: 替换二进制(平台分支)
	if err := in.replaceBinary(newExePath, exePath); err != nil {
		// 替换失败:尝试恢复 .bak
		if in.logger != nil {
			in.logger.Error("UPDATE", "替换二进制失败,尝试回滚: %v", err)
		}
		if rerr := copyFile(bakPath, exePath, 0755); rerr != nil {
			if in.logger != nil {
				in.logger.Error("UPDATE", "回滚失败! .bak 恢复异常: %v (二进制可能损坏,请手动检查)", rerr)
			}
		} else if in.logger != nil {
			in.logger.Warn("UPDATE", "已通过 .bak 回滚二进制")
		}
		_ = os.Remove(bakPath)
		return fmt.Errorf("替换二进制失败: %w", err)
	}

	// Step 3: 替换资源文件(ip2region.xdb + config.yaml.example)
	// 这些文件非关键,失败仅记录日志,不回滚二进制(用户可继续用旧资源)
	newXdb := filepath.Join(extractDir, "ip2region.xdb")
	if _, err := os.Stat(newXdb); err == nil {
		targetXdb := filepath.Join(exeDir, "ip2region.xdb")
		if err := copyFile(newXdb, targetXdb, 0644); err != nil {
			if in.logger != nil {
				in.logger.Warn("UPDATE", "替换 ip2region.xdb 失败(继续): %v", err)
			}
		} else if in.logger != nil {
			in.logger.Info("UPDATE", "已替换 ip2region.xdb")
		}
	}

	newExample := filepath.Join(extractDir, "config.yaml.example")
	if _, err := os.Stat(newExample); err == nil {
		targetExample := filepath.Join(exeDir, "config.yaml.example")
		// config.yaml.example 不存在时才写入,避免覆盖用户自定义的 example(参考性文件)
		if _, err := os.Stat(targetExample); os.IsNotExist(err) {
			if err := copyFile(newExample, targetExample, 0644); err != nil {
				if in.logger != nil {
					in.logger.Warn("UPDATE", "写入 config.yaml.example 失败(继续): %v", err)
				}
			} else if in.logger != nil {
				in.logger.Info("UPDATE", "已写入 config.yaml.example")
			}
		} else if in.logger != nil {
			in.logger.Info("UPDATE", "config.yaml.example 已存在,跳过(不覆盖用户文件)")
		}
	}

	// Step 4: 清理备份(Linux 替换已成功;Windows 需等新进程启动后清理 .old)
	if runtime.GOOS != "windows" {
		_ = os.Remove(bakPath)
	}

	if in.logger != nil {
		in.logger.Info("UPDATE", "二进制替换完成 (耗时 %s)",
			time.Since(installStart).Round(time.Millisecond))
	}
	return nil
}

// replaceBinary 平台分支替换二进制文件
func (in *Installer) replaceBinary(newExePath, exePath string) error {
	if runtime.GOOS == "windows" {
		return in.replaceBinaryWindows(newExePath, exePath)
	}
	return in.replaceBinaryLinux(newExePath, exePath)
}

// replaceBinaryLinux Linux 二进制替换
// 1. 写入新二进制到 exe+".new"
// 2. 原子 os.Rename 覆盖运行中文件(Linux 支持重命名覆盖运行中文件)
// 3. 赋予可执行权限
func (in *Installer) replaceBinaryLinux(newExePath, exePath string) error {
	tmpExe := exePath + ".new"
	// 复制新二进制到 .new
	if err := copyFile(newExePath, tmpExe, 0755); err != nil {
		return fmt.Errorf("写入新二进制失败: %w", err)
	}
	if in.logger != nil {
		in.logger.Info("UPDATE", "已写入新二进制到临时文件: %s", tmpExe)
	}
	// 原子 rename 覆盖当前 exe
	if err := os.Rename(tmpExe, exePath); err != nil {
		_ = os.Remove(tmpExe)
		return fmt.Errorf("原子替换二进制失败: %w", err)
	}
	if in.logger != nil {
		in.logger.Info("UPDATE", "已原子替换运行中二进制: %s", exePath)
	}
	return nil
}

// replaceBinaryWindows Windows 二进制替换
// Windows 不允许写入运行中的 .exe,但允许重命名运行中的 .exe
// 1. 重命名当前 exe 为 exe+".old"(Windows 特性)
// 2. 写入新 exe
// 3. 写入 update_state.json,标记新进程启动后清理 .old 文件
func (in *Installer) replaceBinaryWindows(newExePath, exePath string) error {
	oldExe := exePath + ".old"
	// 如果上次更新遗留 .old 文件(异常情况,例如上次替换后新进程未启动就崩溃),先清理
	if err := os.Remove(oldExe); err == nil {
		if in.logger != nil {
			in.logger.Warn("UPDATE", "清理上次更新遗留的 .old 文件: %s", oldExe)
		}
	}

	// 重命名当前 exe 为 .old
	if err := os.Rename(exePath, oldExe); err != nil {
		return fmt.Errorf("重命名当前 exe 为 .old 失败: %w", err)
	}
	if in.logger != nil {
		in.logger.Info("UPDATE", "已重命名当前 exe 为 .old: %s", oldExe)
	}

	// 写入新 exe
	if err := copyFile(newExePath, exePath, 0755); err != nil {
		// 写入失败:尝试恢复 .old
		if rerr := os.Rename(oldExe, exePath); rerr == nil {
			if in.logger != nil {
				in.logger.Warn("UPDATE", "写入新 exe 失败,已恢复 .old: %v", err)
			}
		} else if in.logger != nil {
			in.logger.Error("UPDATE", "写入新 exe 失败且恢复 .old 也失败! 二进制可能损坏: write_err=%v restore_err=%v", err, rerr)
		}
		return fmt.Errorf("写入新 exe 失败: %w", err)
	}
	if in.logger != nil {
		in.logger.Info("UPDATE", "已写入新 exe: %s", exePath)
	}

	// 写入 update_state.json,标记新进程启动后清理 .old 文件
	statePath := filepath.Join(filepath.Dir(exePath), updateStateFilename)
	state := &UpdateState{
		OldFiles:    []string{oldExe},
		UpdatedAt:   nowTime(),
		Success:     true,
		LastVersion: "", // 由 Manager 在调用 Install 前注入更准确(此处仅记录清理任务)
	}
	if err := SaveState(statePath, state); err != nil {
		if in.logger != nil {
			in.logger.Warn("UPDATE", "写入 update_state.json 失败(继续,但 .old 文件需手动清理): %v", err)
		}
	} else if in.logger != nil {
		in.logger.Info("UPDATE", "已写入 update_state.json,标记新进程启动后清理 .old 文件")
	}
	return nil
}

// Cleanup 清理解压目录(更新成功/失败后调用)
func (in *Installer) Cleanup(extractDir string) {
	if extractDir == "" {
		return
	}
	_ = os.RemoveAll(extractDir)
}

// findNewBinary 在解压目录中查找新二进制文件
// Linux: cf-speedtest / cf-speedtest-{version}
// Windows: cf-speedtest.exe / cf-speedtest-windows-amd64.exe
func findNewBinary(extractDir string) (string, error) {
	candidates := []string{}
	if runtime.GOOS == "windows" {
		candidates = []string{
			"cf-speedtest.exe",
			"cf-speedtest-windows-amd64.exe",
		}
	} else {
		candidates = []string{
			"cf-speedtest",
			"cf-speedtest-linux-amd64",
		}
	}

	// 优先在解压目录根查找
	for _, name := range candidates {
		p := filepath.Join(extractDir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}

	// 解压目录根未找到,在子目录中查找(例如 tar.gz 可能解压出 cf-speedtest-1.2.0/ 子目录)
	var found string
	_ = filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		for _, name := range candidates {
			if base == name {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	if found != "" {
		return found, nil
	}

	return "", fmt.Errorf("解压目录中未找到新二进制文件(期望: %v)", candidates)
}

// copyFile 复制文件(指定权限),完成后 fsync 确保数据落盘
func copyFile(src, dst string, mode os.FileMode) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer dstF.Close()

	if _, err := io.Copy(dstF, srcF); err != nil {
		return err
	}
	// Sync 失败视为复制失败(避免数据未落盘的"假成功")
	if err := dstF.Sync(); err != nil {
		return fmt.Errorf("fsync 失败: %w", err)
	}
	return nil
}
