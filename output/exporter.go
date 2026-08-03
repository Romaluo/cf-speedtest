package output

import (
	"encoding/csv"
	"fmt"
	"os"
	"text/tabwriter"

	"cf-speedtest/model"
	"cf-speedtest/scorer"
)

func ExportCSV(results []model.IPResult, filepath string) error {
	f, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"IP:Port#Country", "Latency (ms)", "Loss Rate (%)", "Bandwidth (MB/s)", "Download Speed (MB/s)", "Score"})

	for _, r := range results {
		w.Write([]string{
			fmt.Sprintf("%s:%d#%s", r.IP, r.Port, r.CountryCode),
			fmt.Sprintf("%.2f", float64(r.TCPLatencyAvg.Milliseconds())),
			fmt.Sprintf("%.1f", r.TCPLossRate*100),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.2f", r.DownloadSpeed),
			fmt.Sprintf("%.2f", r.Score),
		})
	}
	return nil
}

func PrintTable(results []model.IPResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "IP:Port#Country\tLatency(ms)\tLoss(%)\tBandwidth(MB/s)\tDownload(MB/s)\tScore")
	fmt.Fprintln(w, "----------------------\t-----------\t-------\t---------------\t---------------\t-----")

	for _, r := range results {
		fmt.Fprintf(w, "%s:%d#%s\t%.2f\t%.1f\t%.2f\t%.2f\t%.2f\n",
			r.IP,
			r.Port,
			r.CountryCode,
			float64(r.TCPLatencyAvg.Milliseconds()),
			r.TCPLossRate*100,
			r.DownloadSpeed,
			r.DownloadSpeed,
			r.Score,
		)
	}
	w.Flush()
}

// ExportIPTxt 导出 IP.txt 文件
// 格式: IP:端口#国家代码 带宽 Mbps 延迟 ms
// 示例: 211.49.57.175:443#KR 16.94 Mbps 165.15 ms
func ExportIPTxt(results []model.IPResult, filepath string, count int) error {
	f, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	for i := 0; i < count && i < len(results); i++ {
		r := results[i]
		countryCode := r.CountryCode
		if countryCode == "" {
			countryCode = "-"
		}
		latencyMs := float64(r.TCPLatencyAvg.Milliseconds())
		bandwidthMbps := r.DownloadSpeed * 8 // MB/s 转换为 Mbps
		fmt.Fprintf(f, "%s:%d#%s %s %.2f Mbps %.2f ms\n",
			r.IP, r.Port, countryCode, scorer.GetQualityGrade(r.Score), bandwidthMbps, latencyMs)
	}
	return nil
}
