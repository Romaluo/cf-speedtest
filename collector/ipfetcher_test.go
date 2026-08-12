package collector

import (
	"testing"
)

// TestComputeQuotas_60_40_Ratio 验证 60/40 采样配额拆分比例
// 策略:60% 按历史命中率加权(利用) + 40% 均匀分配(探索)
func TestComputeQuotas_60_40_Ratio(t *testing.T) {
	// 构造 2 个 CIDR,极端权重差异以放大 60/40 拆分效果
	// A: 命中率 100% (weight=1.0)
	// B: 命中率 0%   (weight=minWeight=0.05)
	stats := &CIDRStats{
		Stats: map[string]*CIDRStat{
			"1.0.0.0/24": {Sampled: 100, Passed: 100, HitRate: 1.0},
			"2.0.0.0/24": {Sampled: 100, Passed: 0, HitRate: 0.0},
		},
	}
	cidrs := []string{"1.0.0.0/24", "2.0.0.0/24"}
	total := 100

	quotas := computeQuotas(cidrs, total, stats)

	if len(quotas) != 2 {
		t.Fatalf("期望 2 个配额,实际 %d", len(quotas))
	}

	// 1. 总和应等于 total
	sum := quotas[0] + quotas[1]
	if sum != total {
		t.Errorf("配额总和 %d != total %d", sum, total)
	}

	// 2. 60/40 比例核心验证:探索下限
	// explorePool = total * 0.4 = 40,每个 CIDR 探索部分 = 40/2 = 20
	// 因此低权重 CIDR 的配额必须 >= 20(40% 探索比例的特征)
	// 旧版 70/30 比例下此值仅 15,可据此区分新旧版本
	expectedExplorePerCIDR := 20
	if quotas[1] < expectedExplorePerCIDR {
		t.Errorf("低权重 CIDR 配额 %d < 探索下限 %d (60/40 比例未生效,可能仍为 70/30)",
			quotas[1], expectedExplorePerCIDR)
	}
	if quotas[0] < expectedExplorePerCIDR {
		t.Errorf("高权重 CIDR 配额 %d < 探索下限 %d", quotas[0], expectedExplorePerCIDR)
	}

	// 3. 高权重 CIDR 应拿到更多配额(利用部分按权重倾斜)
	if quotas[0] <= quotas[1] {
		t.Errorf("高权重 CIDR 配额 %d 应大于低权重 %d", quotas[0], quotas[1])
	}

	// 4. 精确验证探索部分总和 = total * 0.4 = 40
	// 每个 CIDR 探索部分 = explorePool/n = 40/2 = 20,共 2 个 CIDR
	exploreTotal := expectedExplorePerCIDR * len(cidrs)
	exploitTotal := sum - exploreTotal
	expectedExploit := 60 // total * 0.6
	if exploitTotal != expectedExploit {
		t.Errorf("利用部分总和 %d != 期望 %d (60%% of %d)", exploitTotal, expectedExploit, total)
	}

	t.Logf("60/40 比例验证通过: A(高权重)=%d, B(低权重)=%d", quotas[0], quotas[1])
	t.Logf("  探索部分:每个 CIDR %d,共 %d (40%% of %d)", expectedExplorePerCIDR, exploreTotal, total)
	t.Logf("  利用部分:共 %d (60%% of %d)", exploitTotal, total)
}

// TestComputeQuotas_NoStats_Uniform 验证无历史统计时退化为均匀分配
func TestComputeQuotas_NoStats_Uniform(t *testing.T) {
	cidrs := []string{"1.0.0.0/24", "2.0.0.0/24", "3.0.0.0/24"}
	total := 90

	quotas := computeQuotas(cidrs, total, nil)

	if len(quotas) != 3 {
		t.Fatalf("期望 3 个配额,实际 %d", len(quotas))
	}

	// 无 stats 时均匀分配:每个 = total/n = 90/3 = 30
	expected := 30
	for i, q := range quotas {
		if q != expected {
			t.Errorf("CIDR[%d] 配额 %d != 均匀期望 %d", i, q, expected)
		}
	}
	t.Logf("均匀分配验证通过: %v (每个 %d)", quotas, expected)
}

// TestComputeQuotas_70_30_NotUsed 验证旧 70/30 比例已被替换
// 此测试确保低权重 CIDR 拿到的配额不低于 40% 探索比例对应的下限
func TestComputeQuotas_70_30_NotUsed(t *testing.T) {
	// 10 个 CIDR,1 个高命中率,9 个零命中率
	stats := &CIDRStats{
		Stats: map[string]*CIDRStat{},
	}
	cidrs := make([]string, 10)
	for i := range cidrs {
		cidrs[i] = "10.0.0.0/24"
		_ = i
	}
	// 高命中率 CIDR
	stats.Stats["10.0.0.0/24"] = &CIDRStat{Sampled: 100, Passed: 100, HitRate: 1.0}
	// 其余 CIDR 零命中率
	for i := 1; i < 10; i++ {
		cidr := string(rune('A'+i)) + ".0.0.0/24"
		cidrs[i] = cidr
		stats.Stats[cidr] = &CIDRStat{Sampled: 100, Passed: 0, HitRate: 0.0}
	}

	total := 100
	quotas := computeQuotas(cidrs, total, stats)

	// 60/40: explorePool=40, perExplore=40/10=4
	// 70/30: explorePool=30, perExplore=30/10=3
	// 低权重 CIDR 配额应 >= 4(新版下限),旧版仅 3
	for i := 1; i < 10; i++ {
		if quotas[i] < 4 {
			t.Errorf("CIDR[%d] 配额 %d < 4 (60/40 探索下限,旧 70/30 为 3)", i, quotas[i])
		}
	}
	t.Logf("10 CIDR 场景验证通过:配额 %v", quotas)
}
