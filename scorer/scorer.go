package scorer

import (
	"math"
	"sort"
	"strings"

	"cf-speedtest/config"
	"cf-speedtest/geo"
	"cf-speedtest/model"
)

// Score constants for 100-point absolute scoring
const (
	// Latency thresholds (ms)
	LatencyBestMs  = 50.0  // <=50ms 满分
	LatencyWorstMs = 500.0 // >=500ms 零分

	// Bandwidth reference (MB/s) for absolute scoring
	BandwidthBestMBps  = 100.0 // 100MB/s 满分
	BandwidthWorstMBps = 0.1   // 0.1MB/s 零分
)

// QualityGrade 质量分级
const (
	GradeA = "A" // ≥80 优秀
	GradeB = "B" // ≥60 良好
	GradeC = "C" // ≥40 可用
	GradeD = "D" // <40 差
)

// Jitter thresholds (ms) for absolute scoring
const (
	JitterBestMs  = 5.0   // <=5ms 满分
	JitterWorstMs = 100.0 // >=100ms 零分
)

// GetQualityGrade 根据评分返回质量等级
func GetQualityGrade(score float64) string {
	switch {
	case score >= 80:
		return GradeA
	case score >= 60:
		return GradeB
	case score >= 40:
		return GradeC
	default:
		return GradeD
	}
}

// FilterByCountries 按国家/地区代码过滤测速结果
func FilterByCountries(results []model.IPResult, resolver *geo.Resolver, countries []string) []model.IPResult {
	if len(countries) == 0 {
		return results
	}

	countrySet := make(map[string]bool)
	for _, c := range countries {
		countrySet[strings.ToUpper(strings.TrimSpace(c))] = true
	}

	var filtered []model.IPResult
	for _, r := range results {
		if r.Err != nil || r.TCPLossRate >= 1.0 {
			continue
		}

		code := r.CountryCode
		if (code == "" || code == "-") && resolver.IsEnabled() {
			info := resolver.Lookup(r.IP)
			if info != nil {
				if info.CountryCode != "" && info.CountryCode != "0" {
					code = strings.ToUpper(info.CountryCode)
				} else if info.Country != "" && info.Country != "0" {
					code = GetCountryCode(info.Country)
				}
			}
		}

		if countrySet[code] {
			if r.CountryCode == "" || r.CountryCode == "-" {
				r.CountryCode = code
			}
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// SelectHybridTopN 混合模式选择：50%来自手动指定国家，50%来自其他
// results 必须已按评分降序排序
func SelectHybridTopN(results []model.IPResult, countries []string, n int) []model.IPResult {
	if len(results) == 0 || n <= 0 {
		return nil
	}

	countrySet := make(map[string]bool)
	for _, c := range countries {
		countrySet[strings.ToUpper(strings.TrimSpace(c))] = true
	}

	var manualGroup, autoGroup []model.IPResult
	for _, r := range results {
		if countrySet[strings.ToUpper(r.CountryCode)] {
			manualGroup = append(manualGroup, r)
		} else {
			autoGroup = append(autoGroup, r)
		}
	}

	half := n / 2
	otherHalf := n - half // 处理奇数情况

	// 如果某一组不够，用另一组补齐
	if len(manualGroup) < half {
		otherHalf = n - len(manualGroup)
	}
	if len(autoGroup) < otherHalf {
		half = n - len(autoGroup)
	}
	if half > len(manualGroup) {
		half = len(manualGroup)
	}
	if otherHalf > len(autoGroup) {
		otherHalf = len(autoGroup)
	}

	result := make([]model.IPResult, 0, n)
	result = append(result, manualGroup[:half]...)
	result = append(result, autoGroup[:otherHalf]...)

	// 合并后按评分重新排序
	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})
	return result
}

// latencyScore100 绝对100分制：延迟分数
// <=50ms = 100分, >=500ms = 0分, 线性插值
func latencyScore100(latMs float64) float64 {
	if latMs <= LatencyBestMs {
		return 100.0
	}
	if latMs >= LatencyWorstMs {
		return 0.0
	}
	return 100.0 * (LatencyWorstMs - latMs) / (LatencyWorstMs - LatencyBestMs)
}

// lossScore100 绝对100分制：丢包分数
// 0% = 100分, 100% = 0分
func lossScore100(lossRate float64) float64 {
	if lossRate <= 0 {
		return 100.0
	}
	if lossRate >= 1.0 {
		return 0.0
	}
	return 100.0 * (1.0 - lossRate)
}

// bandwidthScore100 绝对100分制：带宽分数
// >=100MB/s = 100分, <=0.1MB/s = 0分, 对数插值
func bandwidthScore100(bwMBps float64) float64 {
	if bwMBps >= BandwidthBestMBps {
		return 100.0
	}
	if bwMBps <= BandwidthWorstMBps {
		return 0.0
	}
	// 对数插值：log(0.1)=-1, log(100)=2, 跨度3
	logVal := math.Log10(bwMBps)
	logMin := math.Log10(BandwidthWorstMBps)
	logMax := math.Log10(BandwidthBestMBps)
	return 100.0 * (logVal - logMin) / (logMax - logMin)
}

// jitterScore100 绝对100分制：HTTP 抖动分数
// <=5ms = 100分, >=100ms = 0分, 线性插值
func jitterScore100(jitterMs float64) float64 {
	if jitterMs <= JitterBestMs {
		return 100.0
	}
	if jitterMs >= JitterWorstMs {
		return 0.0
	}
	return 100.0 * (JitterWorstMs - jitterMs) / (JitterWorstMs - JitterBestMs)
}

// calcScore100 计算单个IP的100分制综合评分
func calcScore100(r *model.IPResult, cfg *config.Config) float64 {
	latMs := float64(r.TCPLatencyAvg.Milliseconds())
	latScore := latencyScore100(latMs)
	losScore := lossScore100(r.TCPLossRate)
	bwScore := bandwidthScore100(r.DownloadSpeed)
	jitScore := jitterScore100(float64(r.HTTPJitter.Milliseconds()))

	// 当权重之和为 0 时退化为等权
	wLat := cfg.WeightLatency
	wLoss := cfg.WeightLoss
	wBw := cfg.WeightBandwidth
	wJit := cfg.WeightJitter
	totalW := wLat + wLoss + wBw + wJit
	if totalW <= 0 {
		wLat, wLoss, wBw, wJit = 0.25, 0.25, 0.25, 0.25
		totalW = 1.0
	}

	score := (wLat*latScore +
		wLoss*losScore +
		wBw*bwScore +
		wJit*jitScore) / totalW

	// 四舍五入到小数点后2位
	return math.Round(score*100) / 100
}

// Score 对结果进行100分制评分，截断为TopN
func Score(results []model.IPResult, cfg *config.Config) []model.IPResult {
	var valid []model.IPResult
	for _, r := range results {
		if r.Err == nil && r.TCPLossRate < 1.0 {
			valid = append(valid, r)
		}
	}

	if len(valid) == 0 {
		return nil
	}

	for i := range valid {
		valid[i].Score = calcScore100(&valid[i], cfg)
	}

	sort.Slice(valid, func(i, j int) bool {
		return valid[i].Score > valid[j].Score
	})

	if len(valid) > cfg.TopN {
		valid = valid[:cfg.TopN]
	}

	return valid
}

// ScoreAllResults 对所有结果进行100分制评分（不截断TopN）
func ScoreAllResults(results []model.IPResult, cfg *config.Config) []model.IPResult {
	var valid []model.IPResult
	for _, r := range results {
		if r.Err == nil && r.TCPLossRate < 1.0 {
			valid = append(valid, r)
		}
	}

	if len(valid) == 0 {
		return nil
	}

	for i := range valid {
		valid[i].Score = calcScore100(&valid[i], cfg)
	}

	return valid
}

// ScoreByISP 按本机运营商分组，每组内独立100分制评分并取TopN
func ScoreByISP(results []model.IPResult, resolver *geo.Resolver, cfg *config.Config) map[string][]model.IPResult {
	grouped := make(map[string][]model.IPResult)

	for _, r := range results {
		if r.Err != nil || r.TCPLossRate >= 1.0 {
			continue
		}

		isp := r.ISP
		if isp == "" {
			isp = "Unknown"
		}

		countryCode := r.CountryCode
		if (countryCode == "" || countryCode == "-") && resolver != nil && resolver.IsEnabled() {
			info := resolver.Lookup(r.IP)
			if info != nil {
				if info.CountryCode != "" && info.CountryCode != "0" {
					countryCode = strings.ToUpper(info.CountryCode)
				} else if info.Country != "" && info.Country != "0" {
					countryCode = GetCountryCode(info.Country)
				}
			}
		}
		if countryCode == "" {
			countryCode = "-"
		}
		r.CountryCode = countryCode

		grouped[isp] = append(grouped[isp], r)
	}

	for isp, items := range grouped {
		for i := range items {
			items[i].Score = calcScore100(&items[i], cfg)
		}

		sort.Slice(items, func(i, j int) bool {
			return items[i].Score > items[j].Score
		})

		if len(items) > cfg.TopN {
			items = items[:cfg.TopN]
		}
		grouped[isp] = items
	}

	return grouped
}

func GetCountryCode(country string) string {
	countryMap := map[string]string{
		"中国":            "CN",
		"United States": "US",
		"日本":            "JP",
		"韩国":            "KR",
		"新加坡":           "SG",
		"香港":            "HK",
		"台湾":            "TW",
		"澳门":            "MO",
		"德国":            "DE",
		"英国":            "UK",
		"法国":            "FR",
		"加拿大":           "CA",
		"澳大利亚":          "AU",
		"俄罗斯":           "RU",
		"印度":            "IN",
		"巴西":            "BR",
		"荷兰":            "NL",
		"瑞典":            "SE",
		"芬兰":            "FI",
		"挪威":            "NO",
		"丹麦":            "DK",
		"瑞士":            "CH",
		"意大利":           "IT",
		"西班牙":           "ES",
		"葡萄牙":           "PT",
		"爱尔兰":           "IE",
		"比利时":           "BE",
		"卢森堡":           "LU",
		"奥地利":           "AT",
		"希腊":            "GR",
		"波兰":            "PL",
		"匈牙利":           "HU",
		"捷克":            "CZ",
		"罗马尼亚":          "RO",
		"保加利亚":          "BG",
		"克罗地亚":          "HR",
		"塞尔维亚":          "RS",
		"土耳其":           "TR",
		"以色列":           "IL",
		"阿联酋":           "AE",
		"沙特阿拉伯":         "SA",
		"卡塔尔":           "QA",
		"科威特":           "KW",
		"阿曼":            "OM",
		"巴林":            "BH",
		"埃及":            "EG",
		"南非":            "ZA",
		"尼日利亚":          "NG",
		"肯尼亚":           "KE",
		"摩洛哥":           "MA",
		"阿根廷":           "AR",
		"墨西哥":           "MX",
		"智利":            "CL",
		"哥伦比亚":          "CO",
		"委内瑞拉":          "VE",
		"秘鲁":            "PE",
		"厄瓜多尔":          "EC",
		"乌拉圭":           "UY",
		"巴拉圭":           "PY",
		"玻利维亚":          "BO",
		"新西兰":           "NZ",
		"马来西亚":          "MY",
		"泰国":            "TH",
		"越南":            "VN",
		"菲律宾":           "PH",
		"印度尼西亚":         "ID",
		"柬埔寨":           "KH",
		"老挝":            "LA",
		"缅甸":            "MM",
		"孟加拉国":          "BD",
		"巴基斯坦":          "PK",
		"阿富汗":           "AF",
		"伊朗":            "IR",
		"伊拉克":           "IQ",
		"叙利亚":           "SY",
		"黎巴嫩":           "LB",
		"约旦":            "JO",
		"巴勒斯坦":          "PS",
		"也门":            "YE",
		"利比亚":           "LY",
		"突尼斯":           "TN",
		"阿尔及利亚":         "DZ",
		"苏丹":            "SD",
		"埃塞俄比亚":         "ET",
		"坦桑尼亚":          "TZ",
		"乌干达":           "UG",
		"卢旺达":           "RW",
		"布隆迪":           "BI",
		"刚果":            "CD",
		"安哥拉":           "AO",
		"加纳":            "GH",
		"科特迪瓦":          "CI",
		"多哥":            "TG",
		"贝宁":            "BJ",
		"尼日尔":           "NE",
		"马里":            "ML",
		"布基纳法索":         "BF",
		"塞内加尔":          "SN",
		"冈比亚":           "GM",
		"毛里塔尼亚":         "MR",
		"塞拉利昂":          "SL",
		"利比里亚":          "LR",
		"几内亚":           "GN",
		"几内亚比绍":         "GW",
		"佛得角":           "CV",
		"圣多美":           "ST",
		"普林西比":          "SH",
		"赤道几内亚":         "GQ",
		"加蓬":            "GA",
		"刚果共和国":         "CG",
		"牙买加":           "JM",
		"海地":            "HT",
		"多米尼加":          "DO",
		"古巴":            "CU",
		"伯利兹":           "BZ",
		"危地马拉":          "GT",
		"洪都拉斯":          "HN",
		"萨尔瓦多":          "SV",
		"尼加拉瓜":          "NI",
		"哥斯达黎加":         "CR",
		"巴拿马":           "PA",
		"波多黎各":          "PR",
	}

	if code, ok := countryMap[country]; ok {
		return code
	}
	if len(country) >= 2 {
		return country[:2]
	}
	return country
}
