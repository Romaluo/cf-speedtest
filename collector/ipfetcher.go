package collector

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cf-speedtest/config"
	"cf-speedtest/model"
)

// Fetcher IP 段获取与解析
type Fetcher struct {
	cfg   *config.Config
	stats *CIDRStats
}

func NewFetcher(cfg *config.Config) *Fetcher {
	return &Fetcher{cfg: cfg}
}

// NewFetcherWithStats 创建带 CIDR 权重统计的 Fetcher
func NewFetcherWithStats(cfg *config.Config, stats *CIDRStats) *Fetcher {
	return &Fetcher{cfg: cfg, stats: stats}
}

// FetchResult 分类拉取结果
type FetchResult struct {
	OfficialTasks []model.Task // 官方地址拉取的IP（CIDR采样，无自带国家代码）
	CustomTasks   []model.Task // 自定义地址拉取的IP（通常带国家代码）
}

// FetchProgress 拉取进度信息（用于实时显示）
type FetchProgress struct {
	Source   string // "official" 或 "custom"
	URL      string // 当前拉取的URL
	Count    int    // 当前URL拉取的IP数量
	Total    int    // 所有URL合计拉取数量
	Official int    // 官方地址累计数量
	Custom   int    // 自定义地址累计数量
}

// FetchCategorized 分类拉取IP，返回官方地址和自定义地址的结果
// onProgress 用于实时报告拉取进度（可为 nil）
// 官方地址: 若 IPv4Enabled=true，按 IPv4Count 随机采样；否则不拉取
// 自定义地址: 完整拉取所有IP（不做采样）
const SAMPLES_PER_CIDR = 10

func (f *Fetcher) FetchCategorized(onProgress func(FetchProgress)) (*FetchResult, error) {
	var mu sync.Mutex
	var official, custom []model.Task
	var officialTotal, customTotal int
	var wg sync.WaitGroup

	// 官方地址拉取（按数量随机采样）
	if f.cfg.IPv4Enabled && f.cfg.IPv4URL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()

			lines, err := f.fetchLines(f.cfg.IPv4URL)
			if err != nil {
				fmt.Printf("[WARN] 获取官方 IP 段失败 %s: %v\n", f.cfg.IPv4URL, err)
				return
			}

			var tasks []model.Task
			if isCIDRFormat(lines) {
				count := f.cfg.IPv4Count
				if count <= 0 {
					count = 100
				}
				if f.stats != nil {
					// 记录本轮采样数并按权重采样
					tasks = sampleCIDRsWeighted(lines, f.cfg.TCPPingPorts, count, f.stats)
					// 记录各 CIDR 本轮采样数到统计
					sampledPerCIDR := make(map[string]int)
					for _, t := range tasks {
						if t.SourceCIDR != "" {
							sampledPerCIDR[t.SourceCIDR]++
						}
					}
					for cidr, cnt := range sampledPerCIDR {
						f.stats.RecordSampled(cidr, cnt)
					}
					fmt.Printf("[INFO] 加权采样完成: %d 个 CIDR, 总采样 %d 个 IP\n", len(sampledPerCIDR), len(tasks))
				} else {
					tasks = sampleCIDRsTotal(lines, f.cfg.TCPPingPorts, count)
				}
			} else {
				tasks = parseIPList(lines, f.cfg.TCPPingPorts)
				if f.cfg.IPv4Count > 0 && len(tasks) > f.cfg.IPv4Count {
					tasks = sampleIPs(tasks, f.cfg.IPv4Count)
				}
			}

			mu.Lock()
			official = tasks
			officialTotal = len(tasks)
			curOfficial, curCustom := officialTotal, customTotal
			mu.Unlock()

			if onProgress != nil {
				onProgress(FetchProgress{
					Source: "official", URL: f.cfg.IPv4URL,
					Count: len(tasks), Total: curOfficial + curCustom,
					Official: curOfficial, Custom: curCustom,
				})
			}
		}()
	}

	// 自定义地址完整拉取（不采样，拉取全部）
	for _, url := range f.cfg.ExtraIPURLs {
		if url == "" {
			continue
		}
		wg.Add(1)
		go func(u string) {
			defer wg.Done()

			lines, err := f.fetchLines(u)
			if err != nil {
				fmt.Printf("[WARN] 获取自定义 IP 段失败 %s: %v\n", u, err)
				return
			}

			var tasks []model.Task
			if isCIDRFormat(lines) {
				tasks = sampleCIDRsFixed(lines, f.cfg.TCPPingPorts, SAMPLES_PER_CIDR)
			} else {
				tasks = parseIPList(lines, f.cfg.TCPPingPorts)
			}

			mu.Lock()
			custom = append(custom, tasks...)
			customTotal += len(tasks)
			curOfficial, curCustom := officialTotal, customTotal
			mu.Unlock()

			if onProgress != nil {
				onProgress(FetchProgress{
					Source: "custom", URL: u,
					Count: len(tasks), Total: curOfficial + curCustom,
					Official: curOfficial, Custom: curCustom,
				})
			}
		}(url)
	}
	wg.Wait()

	fmt.Printf("[INFO] 共解析出 %d 个 IP 地址（官方 %d + 自定义 %d）\n",
		len(official)+len(custom), len(official), len(custom))

	// 端口兜底过滤: 确保只保留用户配置端口列表中的任务
	portSet := make(map[int]bool, len(f.cfg.TCPPingPorts))
	for _, p := range f.cfg.TCPPingPorts {
		portSet[p] = true
	}
	if len(portSet) > 0 {
		official = filterByPorts(official, portSet)
		custom = filterByPorts(custom, portSet)
	}

	// 全局采样控制（仅在未单独配置官方数量时生效）
	if f.cfg.MaxIPs > 0 && len(official)+len(custom) > f.cfg.MaxIPs {
		merged := make([]model.Task, 0, len(official)+len(custom))
		merged = append(merged, official...)
		merged = append(merged, custom...)
		merged = sampleIPs(merged, f.cfg.MaxIPs)
		fmt.Printf("[INFO] 全局采样后剩余 %d 个 IP 地址\n", len(merged))
		// 按来源重新拆分（简单按比例保留）
		official, custom = splitByOrigin(merged, official, custom)
	}

	return &FetchResult{OfficialTasks: official, CustomTasks: custom}, nil
}

// filterByPorts 端口兜底过滤
func filterByPorts(tasks []model.Task, portSet map[int]bool) []model.Task {
	filtered := tasks[:0]
	for _, t := range tasks {
		if portSet[t.Port] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// splitByOrigin 按原始来源拆分采样后的任务（基于IP+Port匹配）
func splitByOrigin(merged, official, custom []model.Task) ([]model.Task, []model.Task) {
	customSet := make(map[string]bool, len(custom))
	for _, t := range custom {
		customSet[fmt.Sprintf("%s:%d", t.IP, t.Port)] = true
	}
	var newOfficial, newCustom []model.Task
	for _, t := range merged {
		key := fmt.Sprintf("%s:%d", t.IP, t.Port)
		if customSet[key] {
			newCustom = append(newCustom, t)
		} else {
			newOfficial = append(newOfficial, t)
		}
	}
	return newOfficial, newCustom
}

// isCIDRFormat 判断行列表是否为 CIDR 格式
func isCIDRFormat(lines []string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 包含 '/' 说明是 CIDR 格式
		if strings.Contains(line, "/") {
			return true
		}
		// 如果第一个有效行包含 ':' 或 '#' 或 只有 IP，则不是 CIDR
		if strings.Contains(line, ":") || strings.Contains(line, "#") {
			return false
		}
		// 尝试解析为 IP，如果是纯 IP 格式
		if ip := net.ParseIP(line); ip != nil {
			return false
		}
		// 其他情况默认按 CIDR 处理
		return true
	}
	return true
}

// parseIPList 解析 IP 列表格式 (IP:port#country 或 IP 格式)
// 端口处理规则:
//  1. IP自带端口且与用户指定端口不一致 → 清除该IP
//  2. IP自带端口且与用户指定端口完全一致 → 保留
//  3. IP未携带端口 → 自动配置用户指定端口后保留
//
// 端口验证严格匹配数字格式及1-65535范围要求
func parseIPList(lines []string, allowedPorts []int) []model.Task {
	// 构建允许端口集合用于快速查找
	portSet := make(map[int]bool, len(allowedPorts))
	for _, p := range allowedPorts {
		portSet[p] = true
	}

	// 内存优化:以 len(lines) 为起始容量减少早期扩容
	tasks := make([]model.Task, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 格式1: IP:port#country (如 101.99.76.88:2053#NL)
		// 格式2: IP:port (如 101.99.76.88:443)
		// 格式3: IP (如 101.99.76.88)

		// 解析 # 后面的国家代码（保留用于后续国家过滤）
		ipPortPart := line
		countryCode := ""
		if idx := strings.Index(line, "#"); idx != -1 {
			ipPortPart = line[:idx]
			countryCode = strings.ToUpper(strings.TrimSpace(line[idx+1:]))
		}

		var ip string
		var port int
		hasPort := false // 严格区分"自带端口"与"无端口"

		if idx := strings.LastIndex(ipPortPart, ":"); idx != -1 {
			hasPort = true
			ip = ipPortPart[:idx]
			portStr := ipPortPart[idx+1:]
			// 严格数字格式验证
			p, err := strconv.Atoi(portStr)
			if err != nil {
				// 端口非数字格式 → 清除该IP
				continue
			}
			// 端口范围验证 (1-65535)
			if p < 1 || p > 65535 {
				continue
			}
			port = p
		} else {
			ip = ipPortPart
		}

		// 验证 IP 格式正确性
		if net.ParseIP(ip) == nil {
			continue
		}

		if hasPort {
			// 规则1&2: IP自带端口
			if portSet[port] {
				// 端口与用户指定完全一致 → 保留
				tasks = append(tasks, model.Task{IP: ip, Port: port, CountryCode: countryCode})
			}
			// 端口与用户指定不一致 → 清除该IP（不添加，直接跳过）
		} else {
			// 规则3: IP未携带端口 → 自动配置用户指定端口
			for _, p := range allowedPorts {
				tasks = append(tasks, model.Task{IP: ip, Port: p, CountryCode: countryCode})
			}
		}
	}
	return tasks
}

// sampleIPs 随机采样指定数量的 IP
func sampleIPs(tasks []model.Task, count int) []model.Task {
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].IP < tasks[j].IP
	})
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	shuffled := make([]model.Task, len(tasks))
	copy(shuffled, tasks)
	r.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	return shuffled[:count]
}

// fetchLines 从 URL 获取所有行（通用）
func (f *Fetcher) fetchLines(url string) ([]string, error) {
	client := &http.Client{
		Timeout: 15 * time.Second,
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// fetchCIDRs 从 URL 获取 CIDR 列表（保留用于兼容）
func (f *Fetcher) fetchCIDRs(url string) ([]string, error) {
	return f.fetchLines(url)
}

// expandCIDRs 将 CIDR 网段展开为具体 IP 任务列表
// 为每个 IP 的每个端口生成一个独立的测速任务
// 注意：此函数会展开全量 IP，可能导致内存爆炸。请使用 sampleCIDRs 代替。
func expandCIDRs(cidrs []string, ports []int) []model.Task {
	var tasks []model.Task
	for _, cidr := range cidrs {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); incIP(ip) {
			ipStr := ip.String()
			for _, port := range ports {
				tasks = append(tasks, model.Task{
					IP:   ipStr,
					Port: port,
				})
			}
		}
	}
	return tasks
}

// sampleCIDRsTotal 从所有 CIDR 中总共采样 count 个 IP
// 按比例分配每个 CIDR 的采样数量，最终合并并随机裁剪到精确的 count
func sampleCIDRsTotal(cidrs []string, ports []int, total int) []model.Task {
	return sampleCIDRsWeighted(cidrs, ports, total, nil)
}

// sampleCIDRsWeighted 按历史命中率权重从各 CIDR 采样 IP
// stats 为 nil 时退化为均匀采样；有统计时按命中率加权分配
// 每个 IP 的 SourceCIDR 字段标记其来源 CIDR
func sampleCIDRsWeighted(cidrs []string, ports []int, total int, stats *CIDRStats) []model.Task {
	if len(cidrs) == 0 || total <= 0 {
		return nil
	}

	// 计算各 CIDR 的采样配额
	quotas := computeQuotas(cidrs, total, stats)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	// 内存优化:已知上界为 total*len(ports),预分配避免循环内扩容
	tasks := make([]model.Task, 0, total*len(ports))

	for i, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		perCIDR := quotas[i]
		if perCIDR <= 0 {
			continue
		}

		// 从该 CIDR 中采样 perCIDR 个 IP
		sampled := sampleFromCIDR(ipNet, perCIDR, rng)
		for _, ipStr := range sampled {
			for _, port := range ports {
				tasks = append(tasks, model.Task{
					IP:         ipStr,
					Port:       port,
					SourceCIDR: cidr,
				})
			}
		}
	}

	// 超出 total 时随机裁剪到精确数量
	if len(tasks) > total {
		tasks = sampleIPs(tasks, total)
	}
	return tasks
}

// computeQuotas 根据权重计算各 CIDR 的采样配额
// 策略：70% 按历史命中率加权（利用），30% 均匀分配（探索）
// 确保每个 CIDR 至少 1 个配额
func computeQuotas(cidrs []string, total int, stats *CIDRStats) []int {
	n := len(cidrs)
	quotas := make([]int, n)

	if stats == nil || n == 0 {
		// 均匀分配
		per := total / n
		if per < 1 {
			per = 1
		}
		for i := range quotas {
			quotas[i] = per
		}
		return quotas
	}

	// 获取权重
	weights := make([]float64, n)
	sumWeight := 0.0
	for i, cidr := range cidrs {
		w := stats.GetWeight(cidr)
		weights[i] = w
		sumWeight += w
	}

	if sumWeight == 0 {
		// 全部无权重，均匀分配
		per := total / n
		if per < 1 {
			per = 1
		}
		for i := range quotas {
			quotas[i] = per
		}
		return quotas
	}

	// 70% 按权重分配，30% 均匀探索
	exploitPool := int(float64(total) * 0.7)
	explorePool := total - exploitPool
	perExplore := explorePool / n
	if perExplore < 1 {
		perExplore = 1
		explorePool = perExplore * n
		exploitPool = total - explorePool
	}

	// 先分配探索部分（每个 CIDR 至少 perExplore 个）
	for i := range quotas {
		quotas[i] = perExplore
	}

	// 再按权重分配利用部分
	remaining := exploitPool
	for i := range cidrs {
		share := int(float64(exploitPool) * weights[i] / sumWeight)
		if share < 0 {
			share = 0
		}
		quotas[i] += share
		remaining -= share
	}

	// 把余数分配给权重最高的 CIDR
	if remaining > 0 {
		bestIdx := 0
		bestWeight := weights[0]
		for i := 1; i < n; i++ {
			if weights[i] > bestWeight {
				bestWeight = weights[i]
				bestIdx = i
			}
		}
		quotas[bestIdx] += remaining
	}

	return quotas
}

// sampleCIDRsFixed 从 CIDR 网段中每个采样固定数量的 IP
// 每个 CIDR 采样 count 个 IP，使用当前时间作为种子保证每次运行结果不同
func sampleCIDRsFixed(cidrs []string, ports []int, perCIDR int) []model.Task {
	if len(cidrs) == 0 || perCIDR <= 0 {
		return nil
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	// 内存优化:已知上界为 len(cidrs)*perCIDR*len(ports),预分配避免循环内扩容
	tasks := make([]model.Task, 0, len(cidrs)*perCIDR*len(ports))

	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		// 从该 CIDR 中采样 perCIDR 个 IP
		sampled := sampleFromCIDR(ipNet, perCIDR, rng)
		for _, ipStr := range sampled {
			for _, port := range ports {
				tasks = append(tasks, model.Task{
					IP:         ipStr,
					Port:       port,
					SourceCIDR: cidr,
				})
			}
		}
	}

	return tasks
}

// subnetSize 计算子网大小（IP 数量）
// 对于 IPv6 或超大子网，返回 -1 表示无法精确计算
func subnetSize(n *net.IPNet) int {
	ones, bits := n.Mask.Size()
	if bits == 0 {
		return 0
	}
	// /31, /32, /127, /128 的特殊处理
	if ones >= bits {
		return 1
	}

	hostBits := bits - ones
	// 避免溢出：对于大子网返回 -1
	if hostBits > 62 {
		return -1 // 表示超大子网，无法精确计算
	}

	size := 1 << uint(hostBits)
	if size < 0 {
		return 0
	}
	return size
}

// sampleFromCIDR 从 CIDR 中随机采样 count 个不同的 IP
func sampleFromCIDR(n *net.IPNet, count int, rng *rand.Rand) []string {
	// 仅支持 IPv4
	if n.IP.To4() == nil {
		return nil
	}

	size := subnetSize(n)
	if size <= 0 {
		return nil
	}

	// 对于很小的子网（如 /32 或 /31），直接展开
	if size <= count {
		tasks := expandCIDRs([]string{n.String()}, nil)
		ips := make([]string, 0, len(tasks))
		for _, t := range tasks {
			ips = append(ips, t.IP)
		}
		return ips
	}

	return sampleFromIPv4CIDR(n, count, rng)
}

// sampleFromIPv4CIDR 从 IPv4 CIDR 采样
func sampleFromIPv4CIDR(n *net.IPNet, count int, rng *rand.Rand) []string {
	seen := make(map[string]struct{}, count)
	result := make([]string, 0, count)
	networkIP := ipToInt(n.IP.Mask(n.Mask))
	maskSize := uint32(subnetSize(n))

	for len(result) < count && len(seen) < int(maskSize) {
		offset := rng.Int63n(int64(maskSize))
		ip := intToIP(networkIP + uint32(offset))
		ipStr := ip.String()
		if _, ok := seen[ipStr]; !ok {
			seen[ipStr] = struct{}{}
			result = append(result, ipStr)
		}
	}
	return result
}

// ipToInt 将 IPv4 地址转换为 uint32
func ipToInt(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// intToIP 将 uint32 转换为 IPv4 地址
func intToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// incIP 将 IP 地址加 1
func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
