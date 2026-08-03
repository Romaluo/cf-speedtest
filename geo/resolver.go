package geo

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

type ISPInfo struct {
	Country     string
	Region      string
	Province    string
	ISP         string
	CountryCode string
}

type Resolver struct {
	searcher      *xdb.Searcher
	mu            sync.RWMutex
	enabled       bool
	httpClient    *http.Client
	localISP      string         // 缓存本机运营商
	localISPReady bool           // 是否已检测本机运营商
	localCountry  string         // 缓存本机国家（用于 CountryCode 归一化参考）
	corrections   *Corrections   // 纠错覆盖层（优先于 xdb）
	traceVerifier *TraceVerifier // cdn-cgi/trace 精准验证器
}

func NewResolver(dbPath string) (*Resolver, error) {
	version, err := xdb.VersionFromIP("0.0.0.0")
	if err != nil {
		return &Resolver{enabled: false, httpClient: &http.Client{Timeout: 10 * time.Second}}, nil
	}

	// 加载整个 xdb 到内存，使用 NewWithBuffer（线程安全，支持并发读取）
	// NewWithFileOnly 基于文件 Seek+Read，非线程安全，并发调用会破坏文件指针
	cBuff, err := xdb.LoadContentFromFile(dbPath)
	if err != nil {
		return &Resolver{enabled: false, httpClient: &http.Client{Timeout: 10 * time.Second}}, nil
	}
	searcher, err := xdb.NewWithBuffer(version, cBuff)
	if err != nil {
		return &Resolver{enabled: false, httpClient: &http.Client{Timeout: 10 * time.Second}}, nil
	}
	return &Resolver{
		searcher:   searcher,
		enabled:    true,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// NewResolverWithCorrections 创建带纠错覆盖层的 Resolver
// xdb 做快速初筛，corrections 优先覆盖 xdb 结果，traceVerifier 用于精准验证
func NewResolverWithCorrections(dbPath, correctionsPath string, tv *TraceVerifier) (*Resolver, error) {
	r, err := NewResolver(dbPath)
	if err != nil {
		return nil, err
	}
	r.corrections = NewCorrections(correctionsPath)
	r.traceVerifier = tv
	return r, nil
}

// GetLocalISP 检测本机所属运营商（通过公网 IP 查询 + ip2region 解析）
// 结果会缓存，多次调用直接返回缓存值
func (r *Resolver) GetLocalISP() string {
	// 双重检查：先无锁读（已就绪时快速返回），未就绪再加锁检测
	// localISPReady/localISP 仅在持锁状态下写入，无锁读为并发安全的只读访问
	r.mu.RLock()
	if r.localISPReady {
		isp := r.localISP
		r.mu.RUnlock()
		return isp
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.localISPReady {
		return r.localISP
	}

	// 先执行检测，获取 ISP 值
	isp := "Unknown"
	if r.enabled {
		publicIP := r.fetchPublicIP()
		if publicIP != "" {
			info := r.lookupLocked(publicIP)
			if info != nil {
				r.localCountry = info.Country
				isp = NormalizeISP(info.ISP)
				if isp == "" || isp == "0" {
					isp = "Unknown"
				}
			}
		}
	}

	// 先赋值，再标记就绪
	r.localISP = isp
	r.localISPReady = true
	return r.localISP
}

// DetectLocalISP 启动时主动检测本机ISP，返回详细信息供日志使用
func (r *Resolver) DetectLocalISP() (isp, country, publicIP string) {
	publicIP = r.fetchPublicIP()
	if publicIP == "" {
		return "Unknown", "Unknown", ""
	}

	isp = "Unknown"
	country = "Unknown"

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.enabled {
		info := r.lookupLocked(publicIP)
		if info != nil {
			country = info.Country
			isp = NormalizeISP(info.ISP)
			if isp == "" || isp == "0" {
				isp = "Unknown"
			}
			if country == "" || country == "0" {
				country = "Unknown"
			}
		}
	}

	// 确保缓存已设置（先赋值再标记就绪，避免竞态条件）
	r.localISP = isp
	r.localCountry = country
	r.localISPReady = true

	return isp, country, publicIP
}

// fetchPublicIP 获取本机公网 IP（多源兜底）
func (r *Resolver) fetchPublicIP() string {
	sources := []string{
		"https://api.ipify.org?format=text",
		"https://icanhazip.com",
		"https://ifconfig.me/ip",
		"http://ip-api.com/line/?fields=query",
	}
	for _, url := range sources {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "cf-speedtest/1.0")
		resp, err := r.httpClient.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if ip != "" && strings.Contains(ip, ".") {
			return ip
		}
	}
	return ""
}

func (r *Resolver) IsEnabled() bool {
	return r.enabled
}

func (r *Resolver) Lookup(ip string) *ISPInfo {
	if !r.enabled {
		return nil
	}

	// 1. 优先检查纠错覆盖层
	if r.corrections != nil {
		if cc, ok := r.corrections.Lookup(ip); ok {
			return &ISPInfo{
				Country:     cc,
				CountryCode: cc,
				ISP:         "0",
			}
		}
	}

	// 2. 回退到 xdb 查询
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.lookupLocked(ip)
}

// LookupWithTrace 查询 IP 归属地：纠错层 → xdb → (可选)trace 验证
// 当 useTrace=true 且 traceVerifier 可用时，对所有 IP 进行 trace 精准验证
// 验证成功的 IP 自动加入纠错层，下次无需再次 trace
func (r *Resolver) LookupWithTrace(ip string, useTrace bool) *ISPInfo {
	if !r.enabled {
		return nil
	}

	// 1. 优先检查纠错覆盖层（已验证过的IP直接返回）
	if r.corrections != nil {
		if cc, ok := r.corrections.Lookup(ip); ok {
			return &ISPInfo{
				Country:     cc,
				CountryCode: cc,
				ISP:         "0",
			}
		}
	}

	// 2. xdb 查询
	info := r.Lookup(ip)
	if info == nil {
		return nil
	}

	// 3. 若开启 trace 验证，对所有 IP 做精准验证（不限于US）
	if useTrace && r.traceVerifier != nil {
		if colo, cc, err := r.traceVerifier.VerifyIP(ip); err == nil && cc != "" {
			// trace 验证成功，使用实际路由国家
			xdbCC := info.CountryCode // 保存 xdb 结果用于比较
			info.CountryCode = cc
			info.Country = cc
			// 持久化到纠错层（与 xdb 结果不同时才记录）
			if r.corrections != nil && cc != xdbCC {
				r.corrections.Add(ip, cc)
			}
			_ = colo
		}
	}

	return info
}

// AddCorrection 手动添加一条纠错记录
func (r *Resolver) AddCorrection(ip, countryCode string) error {
	if r.corrections == nil {
		return nil
	}
	return r.corrections.Add(ip, countryCode)
}

// CorrectionsCount 返回纠错记录数量
func (r *Resolver) CorrectionsCount() int {
	if r.corrections == nil {
		return 0
	}
	return r.corrections.Count()
}

// lookupLocked 执行实际的 xdb 查询（调用方需自行持有锁）
func (r *Resolver) lookupLocked(ip string) *ISPInfo {
	region, err := r.searcher.Search(ip)
	if err != nil {
		return nil
	}

	parts := strings.Split(region, "|")
	if len(parts) < 5 {
		return nil
	}

	// xdb 格式: Country|Region|Province|ISP|CountryCode
	// 例如: "United States|California|0|0|US"
	// 例如: "中国|江苏省|南京市|0|CN"
	return &ISPInfo{
		Country:     parts[0],
		Region:      parts[1],
		Province:    parts[2],
		ISP:         parts[3],
		CountryCode: parts[4],
	}
}

func (r *Resolver) Close() error {
	if r.searcher != nil {
		r.searcher.Close()
	}
	return nil
}

func NormalizeISP(isp string) string {
	isp = strings.TrimSpace(isp)
	if isp == "" {
		return "Unknown"
	}

	isp = strings.ToLower(isp)
	switch {
	case strings.Contains(isp, "电信") || strings.Contains(isp, "ct") || strings.Contains(isp, "china telecom"):
		return "电信"
	case strings.Contains(isp, "联通") || strings.Contains(isp, "unicom") || strings.Contains(isp, "cu"):
		return "联通"
	case strings.Contains(isp, "移动") || strings.Contains(isp, "mobile") || strings.Contains(isp, "cmcc"):
		return "移动"
	case strings.Contains(isp, "铁通") || strings.Contains(isp, "tie"):
		return "铁通"
	case strings.Contains(isp, "教育") || strings.Contains(isp, "edu"):
		return "教育网"
	case strings.Contains(isp, "网通"):
		return "联通"
	case strings.Contains(isp, "长城") || strings.Contains(isp, "greatwall"):
		return "长城宽带"
	default:
		return isp
	}
}
