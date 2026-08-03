package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// CIDRStat 单个 CIDR 的历史统计
type CIDRStat struct {
	Sampled int     `json:"sampled"` // 累计采样数
	Passed  int     `json:"passed"`  // 累计通过纠错验证+国家筛选数
	HitRate float64 `json:"hit_rate"` // 命中率 = Passed / Sampled
}

// CIDRStats CIDR 命中率统计器，用于动态调整采样权重
// 原理：纠错验证后，产出目标国家IP多的CIDR下次获得更高采样权重
type CIDRStats struct {
	Stats map[string]*CIDRStat `json:"stats"`
	Path  string               `json:"-"`
	mu    sync.Mutex
}

const (
	minWeight     = 0.05  // 最低权重（确保低命中率CIDR仍被探索）
	defaultWeight = 0.10  // 新CIDR默认权重
	decayFactor   = 0.8   // 历史数据衰减因子（新数据占20%，历史占80%）
)

// NewCIDRStats 创建统计器，从文件加载历史数据
func NewCIDRStats(path string) *CIDRStats {
	cs := &CIDRStats{
		Stats: make(map[string]*CIDRStat),
		Path:  path,
	}
	if path != "" {
		cs.load()
	}
	return cs
}

// load 从文件加载历史统计
func (cs *CIDRStats) load() error {
	data, err := os.ReadFile(cs.Path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cs)
}

// Save 持久化到文件
func (cs *CIDRStats) Save() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.Path, data, 0644)
}

// GetWeight 获取 CIDR 的采样权重
// 新CIDR返回 defaultWeight，已知CIDR返回 max(HitRate, minWeight)
func (cs *CIDRStats) GetWeight(cidr string) float64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	stat, ok := cs.Stats[cidr]
	if !ok || stat.Sampled == 0 {
		return defaultWeight
	}
	rate := float64(stat.Passed) / float64(stat.Sampled)
	if rate < minWeight {
		return minWeight
	}
	return rate
}

// RecordSampled 记录某CIDR本轮采样数（指数衰减合并历史）
func (cs *CIDRStats) RecordSampled(cidr string, count int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	stat, ok := cs.Stats[cidr]
	if !ok {
		stat = &CIDRStat{}
		cs.Stats[cidr] = stat
	}
	// 衰减历史数据后累加新数据
	stat.Sampled = int(float64(stat.Sampled)*decayFactor) + count
	cs.updateRate(stat)
}

// RecordPassed 记录某CIDR本轮通过验证数（指数衰减合并历史）
func (cs *CIDRStats) RecordPassed(cidr string, count int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	stat, ok := cs.Stats[cidr]
	if !ok {
		stat = &CIDRStat{}
		cs.Stats[cidr] = stat
	}
	// 衰减历史数据后累加新数据
	stat.Passed = int(float64(stat.Passed)*decayFactor) + count
	cs.updateRate(stat)
}

// updateRate 更新命中率
func (cs *CIDRStats) updateRate(stat *CIDRStat) {
	if stat.Sampled > 0 {
		stat.HitRate = float64(stat.Passed) / float64(stat.Sampled)
	} else {
		stat.HitRate = 0
	}
}

// GetWeights 批量获取 CIDR 列表对应的权重
func (cs *CIDRStats) GetWeights(cidrs []string) map[string]float64 {
	weights := make(map[string]float64, len(cidrs))
	for _, c := range cidrs {
		weights[c] = cs.GetWeight(c)
	}
	return weights
}

// Summary 返回可读的统计摘要（用于日志）
func (cs *CIDRStats) Summary() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	type entry struct {
		CIDR    string
		Sampled int
		Passed  int
		Rate    float64
	}
	var entries []entry
	for cidr, stat := range cs.Stats {
		entries = append(entries, entry{cidr, stat.Sampled, stat.Passed, stat.HitRate})
	}
	// 按命中率降序
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Rate > entries[j].Rate
	})

	result := fmt.Sprintf("CIDR统计 (%d 个):\n", len(entries))
	for _, e := range entries {
		result += fmt.Sprintf("  %s: 采样%d, 通过%d, 命中率%.1f%%\n",
			e.CIDR, e.Sampled, e.Passed, e.Rate*100)
	}
	return result
}
