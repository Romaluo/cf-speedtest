package geo

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Corrections IP 归属地纠错覆盖层
// 作为 xdb 数据库的补充：xdb 查询结果可能过时（如 Cloudflare IP 注册地≠实际路由地）
// 纠错文件格式: 每行 "IP #国家代码"，# 开头为注释
type Corrections struct {
	filePath string
	mu       sync.RWMutex
	data     map[string]string // IP -> 国家代码
}

// NewCorrections 加载纠错文件（文件不存在则创建空 map）
func NewCorrections(filePath string) *Corrections {
	c := &Corrections{
		filePath: filePath,
		data:     make(map[string]string),
	}
	c.load()
	return c
}

// load 从文件加载纠错数据
func (c *Corrections) load() {
	f, err := os.Open(c.filePath)
	if err != nil {
		return // 文件不存在是正常情况
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 格式: "IP #CC" 或 "IP #CC (colo=XXX, ...)"
		idx := strings.Index(line, "#")
		if idx == -1 {
			continue
		}
		ip := strings.TrimSpace(line[:idx])
		rest := strings.TrimSpace(line[idx+1:])
		// 提取国家代码（取第一个空格前的部分）
		if spaceIdx := strings.Index(rest, " "); spaceIdx != -1 {
			rest = rest[:spaceIdx]
		}
		cc := strings.ToUpper(rest)
		if ip != "" && cc != "" {
			c.data[ip] = cc
		}
	}
}

// Lookup 查询纠错数据，返回国家代码和是否命中
func (c *Corrections) Lookup(ip string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cc, ok := c.data[ip]
	return cc, ok
}

// Add 添加一条纠错记录并持久化到文件
func (c *Corrections) Add(ip, countryCode string) error {
	c.mu.Lock()
	c.data[ip] = strings.ToUpper(countryCode)
	c.mu.Unlock()
	return c.append(ip, countryCode)
}

// AddBatch 批量添加纠错记录并持久化
func (c *Corrections) AddBatch(entries map[string]string) error {
	c.mu.Lock()
	for ip, cc := range entries {
		c.data[ip] = strings.ToUpper(cc)
	}
	c.mu.Unlock()
	return c.rewrite()
}

// append 追加一条记录到文件末尾
func (c *Corrections) append(ip, countryCode string) error {
	f, err := os.OpenFile(c.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开纠错文件失败: %w", err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s #%s\n", ip, strings.ToUpper(countryCode))
	return err
}

// rewrite 全量重写纠错文件
func (c *Corrections) rewrite() error {
	f, err := os.Create(c.filePath)
	if err != nil {
		return fmt.Errorf("重写纠错文件失败: %w", err)
	}
	defer f.Close()

	c.mu.RLock()
	defer c.mu.RUnlock()

	for ip, cc := range c.data {
		fmt.Fprintf(f, "%s #%s\n", ip, cc)
	}
	return nil
}

// Count 返回纠错记录数量
func (c *Corrections) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
