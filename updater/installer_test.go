package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsPathSafe 安全路径检测:基础合法路径应通过
func TestIsPathSafe(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"根目录自身", base, true},
		{"一级子文件", filepath.Join(base, "file.txt"), true},
		{"一级子目录文件", filepath.Join(base, "sub", "file.txt"), true},
		{"多级嵌套", filepath.Join(base, "a", "b", "c", "d.txt"), true},
		{"父目录穿越", filepath.Join(base, "..", "evil.txt"), false},
		{"嵌套父目录穿越", filepath.Join(base, "sub", "..", "..", "evil.txt"), false},
		{"隐藏父目录穿越", filepath.Join(base, "sub/../../evil.txt"), false},
		{"完全外部路径", "/tmp/evil.txt" + strings.Repeat("x", 20), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 跳过完全外部路径与 base 重叠的偶发场景(基于 t.TempDir 的实际路径)
			if !strings.HasPrefix(tc.target, base) && tc.want {
				t.Skip("target 不在 base 下,跳过")
			}
			got := isPathSafe(base, tc.target)
			if got != tc.want {
				t.Errorf("isPathSafe(%q, %q) = %v, want %v", base, tc.target, got, tc.want)
			}
		})
	}
}

// TestIsPathSafe_WindowsStyle Windows 风格路径穿越(即使在 Linux 也应被检测)
// 注意:这些路径在 Linux 上会被 filepath.Abs 当作相对路径处理,
// 但通过 ../ 前缀的检测仍应生效
func TestIsPathSafe_WindowsStyle(t *testing.T) {
	base := t.TempDir()
	// 模拟 zip slip 攻击:目标文件路径试图跳出 base
	evilTargets := []string{
		filepath.Join(base, "..", "evil.exe"),
		filepath.Join(base, "..", "..", "evil.exe"),
	}
	for _, target := range evilTargets {
		if isPathSafe(base, target) {
			t.Errorf("Windows 风格路径穿越未被检测: base=%q target=%q", base, target)
		}
	}
}

// TestWriteFileWithCount_NormalWrite 正常写入应返回正确字节数
func TestWriteFileWithCount_NormalWrite(t *testing.T) {
	dest := t.TempDir()
	target := filepath.Join(dest, "out.bin")
	data := []byte("hello world")

	n, err := writeFileWithCount(target, strings.NewReader(string(data)), 0644)
	if err != nil {
		t.Fatalf("writeFileWithCount 失败: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("写入字节数不匹配: got %d, want %d", n, len(data))
	}
	// 验证文件内容
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("文件内容不匹配: got %q, want %q", got, data)
	}
}

// TestWriteFileWithCount_EmptyInput 空输入应返回 0 字节
func TestWriteFileWithCount_EmptyInput(t *testing.T) {
	dest := t.TempDir()
	target := filepath.Join(dest, "empty.bin")
	n, err := writeFileWithCount(target, strings.NewReader(""), 0644)
	if err != nil {
		t.Fatalf("writeFileWithCount 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空写入字节数应为 0, got %d", n)
	}
}

// TestExtractTarGz_PathTraversalAttack 路径穿越攻击应被拒绝
func TestExtractTarGz_PathTraversalAttack(t *testing.T) {
	dest := t.TempDir()
	archivePath := filepath.Join(dest, "evil.tar.gz")

	// 构造恶意 tar.gz:包含一个试图跳出 destDir 的文件
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("创建 tar.gz 失败: %v", err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	// 添加恶意条目: ../../evil.txt
	hdr := &tar.Header{
		Name:     "../../evil.txt",
		Mode:     0644,
		Size:     int64(len("evil")),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader 失败: %v", err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	tw.Close()
	gw.Close()

	// 解压到子目录 extract/
	extractDir := filepath.Join(dest, "extract")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		t.Fatalf("MkdirAll 失败: %v", err)
	}
	err = extractTarGz(archivePath, extractDir)
	if err == nil {
		t.Fatalf("路径穿越攻击未被检测到,extractTarGz 应返回错误")
	}
	if !strings.Contains(err.Error(), "路径穿越") {
		t.Errorf("错误信息应包含'路径穿越',got: %v", err)
	}
	// 验证 evil.txt 未被写到 extract/ 之外
	evilPath := filepath.Join(dest, "evil.txt")
	if _, err := os.Stat(evilPath); err == nil {
		t.Errorf("路径穿越攻击成功:恶意文件被写到 %s", evilPath)
	}
}

// TestExtractTarGz_SizeMismatch 写入字节数与 header 声明不一致应被检测
func TestExtractTarGz_SizeMismatch(t *testing.T) {
	dest := t.TempDir()
	archivePath := filepath.Join(dest, "mismatch.tar.gz")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("创建 tar.gz 失败: %v", err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	// header 声明 100 字节,但实际写入 5 字节
	hdr := &tar.Header{
		Name:     "short.txt",
		Mode:     0644,
		Size:     100,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader 失败: %v", err)
	}
	if _, err := tw.Write([]byte("short")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	tw.Close()
	gw.Close()

	extractDir := filepath.Join(dest, "extract")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		t.Fatalf("MkdirAll 失败: %v", err)
	}
	err = extractTarGz(archivePath, extractDir)
	if err == nil {
		t.Fatalf("字节数不匹配未被检测到,extractTarGz 应返回错误")
	}
}

// TestExtractZip_PathTraversalAttack zip 路径穿越攻击应被拒绝
func TestExtractZip_PathTraversalAttack(t *testing.T) {
	if runtime.GOOS != "windows" {
		// zip 测试在所有平台都应通过(防御性测试)
	}
	dest := t.TempDir()
	archivePath := filepath.Join(dest, "evil.zip")

	// 构造恶意 zip
	zf, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("创建 zip 失败: %v", err)
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)
	w, err := zw.Create("../../evil.txt")
	if err != nil {
		t.Fatalf("zip Create 失败: %v", err)
	}
	if _, err := w.Write([]byte("evil")); err != nil {
		t.Fatalf("zip Write 失败: %v", err)
	}
	zw.Close()

	extractDir := filepath.Join(dest, "extract")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		t.Fatalf("MkdirAll 失败: %v", err)
	}
	err = extractZip(archivePath, extractDir)
	if err == nil {
		t.Fatalf("路径穿越攻击未被检测到,extractZip 应返回错误")
	}
}

// TestCopyFile_NormalCopy 正常复制应成功且内容一致
func TestCopyFile_NormalCopy(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	srcPath := filepath.Join(src, "src.txt")
	dstPath := filepath.Join(dst, "dst.txt")
	data := "test content for copy"
	if err := os.WriteFile(srcPath, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile 失败: %v", err)
	}
	if err := copyFile(srcPath, dstPath, 0644); err != nil {
		t.Fatalf("copyFile 失败: %v", err)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("读取 dst 失败: %v", err)
	}
	if string(got) != data {
		t.Errorf("复制内容不一致: got %q, want %q", got, data)
	}
}

// TestCopyFile_SrcNotExist 源文件不存在应返回错误
func TestCopyFile_SrcNotExist(t *testing.T) {
	err := copyFile("/nonexistent/src.txt", "/tmp/dst.txt", 0644)
	if err == nil {
		t.Errorf("源文件不存在时 copyFile 应返回错误")
	}
}

// TestStateSaveLoadAtomic 原子写入的 state 文件应可正常加载
func TestStateSaveLoadAtomic(t *testing.T) {
	dest := t.TempDir()
	statePath := filepath.Join(dest, "update_state.json")
	state := &UpdateState{
		OldFiles:    []string{"/tmp/old.exe"},
		LastVersion: "1.2.0",
		UpdatedAt:   nowTime(),
		Success:     true,
	}
	if err := SaveState(statePath, state); err != nil {
		t.Fatalf("SaveState 失败: %v", err)
	}
	loaded, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState 失败: %v", err)
	}
	if loaded == nil {
		t.Fatalf("loaded 不应为 nil")
	}
	if loaded.LastVersion != "1.2.0" {
		t.Errorf("LastVersion 不匹配: got %q, want %q", loaded.LastVersion, "1.2.0")
	}
	if !loaded.Success {
		t.Errorf("Success 应为 true")
	}
}

// TestLoadState_NotExist 文件不存在时返回 nil + nil
func TestLoadState_NotExist(t *testing.T) {
	loaded, err := LoadState("/nonexistent/path/update_state.json")
	if err != nil {
		t.Errorf("文件不存在时不应返回错误, got: %v", err)
	}
	if loaded != nil {
		t.Errorf("文件不存在时 loaded 应为 nil")
	}
}

// TestDeleteState_Idempotent 删除不存在的文件应幂等返回 nil
func TestDeleteState_Idempotent(t *testing.T) {
	if err := DeleteState("/nonexistent/path/update_state.json"); err != nil {
		t.Errorf("DeleteState 应幂等返回 nil, got: %v", err)
	}
}

// TestSaveState_NilState 传入 nil 应返回错误
func TestSaveState_NilState(t *testing.T) {
	dest := t.TempDir()
	statePath := filepath.Join(dest, "state.json")
	if err := SaveState(statePath, nil); err == nil {
		t.Errorf("传入 nil state 应返回错误")
	}
}

// TestCompareVersions 版本号比较
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.2.0", "1.1.0", 1},
		{"1.1.0", "1.2.0", -1},
		{"v1.2.0", "1.1.0", 1},
		{"V1.2.0", "1.1.0", 1},
		{"1.2.0-beta", "1.2.0", 0}, // 预发布标识被忽略
		{"1.2.0+build5", "1.2.0", 0},
		{"2.0.0", "1.99.99", 1},
		{"1.10.0", "1.9.0", 1}, // 数字比较,非字符串比较
		{"", "", 0},
		{"1", "1.0", 0},
		{"1.0.1", "1.0", 1},
	}
	for _, tc := range cases {
		got := compareVersions(tc.a, tc.b)
		// 规范化比较结果(只关心符号)
		if tc.want == 0 && got != 0 {
			t.Errorf("compareVersions(%q, %q) = %d, want 0", tc.a, tc.b, got)
		} else if tc.want > 0 && got <= 0 {
			t.Errorf("compareVersions(%q, %q) = %d, want > 0", tc.a, tc.b, got)
		} else if tc.want < 0 && got >= 0 {
			t.Errorf("compareVersions(%q, %q) = %d, want < 0", tc.a, tc.b, got)
		}
	}
}

// TestParseVersion 各种版本号格式解析
func TestParseVersion(t *testing.T) {
	cases := []struct {
		input string
		want  []int
	}{
		{"1.2.3", []int{1, 2, 3}},
		{"v1.2.3", []int{1, 2, 3}},
		{"V1.2.3", []int{1, 2, 3}},
		{"1.2.3-beta", []int{1, 2, 3}},
		{"1.2.3+build5", []int{1, 2, 3}},
		{"1.2", []int{1, 2}},
		{"1", []int{1}},
		{"", []int{0}}, // 空字符串被 split 为 [""],解析为 [0]
		{"v1.10.0", []int{1, 10, 0}},
	}
	for _, tc := range cases {
		got := parseVersion(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("parseVersion(%q) 长度不匹配: got %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i, v := range got {
			if v != tc.want[i] {
				t.Errorf("parseVersion(%q)[%d] = %d, want %d", tc.input, i, v, tc.want[i])
			}
		}
	}
}

// TestFormatSpeed 速度格式化
func TestFormatSpeed(t *testing.T) {
	cases := []struct {
		speed float64
		want  string
	}{
		{0, ""},
		{-1, ""},
		{512, "512 B/s"},
		{2048, "2.00 KB/s"},
		{1024 * 1024, "1.00 MB/s"},
		{1.5 * 1024 * 1024, "1.50 MB/s"},
	}
	for _, tc := range cases {
		got := formatSpeed(tc.speed)
		if got != tc.want {
			t.Errorf("formatSpeed(%v) = %q, want %q", tc.speed, got, tc.want)
		}
	}
}

// TestFormatDuration 持续时间格式化
func TestFormatDuration(t *testing.T) {
	// 边界条件:0 和负数应返回空字符串
	if got := formatDuration(0); got != "" {
		t.Errorf("formatDuration(0) = %q, want empty", got)
	}
	if got := formatDuration(-1); got != "" {
		t.Errorf("formatDuration(-1) = %q, want empty", got)
	}
	// 30 秒
	if got := formatDuration(30 * time.Second); got != "00:30" {
		t.Errorf("formatDuration(30s) = %q, want %q", got, "00:30")
	}
	// 1 分 30 秒
	if got := formatDuration(90 * time.Second); got != "01:30" {
		t.Errorf("formatDuration(90s) = %q, want %q", got, "01:30")
	}
	// 1 小时 1 分 1 秒
	if got := formatDuration(3661 * time.Second); got != "01:01:01" {
		t.Errorf("formatDuration(3661s) = %q, want %q", got, "01:01:01")
	}
}
