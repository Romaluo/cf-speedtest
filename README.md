# CF-SpeedTest

<div align="center">

**Cloudflare CDN 全球 IP 智能测速优选系统**

*自动采集 Cloudflare IP 段，多维度测速评分，将最优 IP 推送至 Cloudflare DNS 与 GitHub 仓库*

[![Go Version](https://img.shields.io/badge/Go-1.21+-informational)](https://go.dev/doc/devel/release)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)]

</div>

---

## 目录

- [功能特性](#功能特性)
- [技术架构](#技术架构)
- [环境要求](#环境要求)
- [安装步骤](#安装步骤)
- [快速开始](#快速开始)
- [配置指南](#配置指南)
- [使用教程](#使用教程)
- [项目结构](#项目结构)
- [工作流程](#工作流程)
- [常见问题（FAQ）](#常见问题faq)
- [故障排除](#故障排除)
- [性能调优](#性能调优)
- [贡献规范](#贡献规范)
- [开源许可](#开源许可)

---

## 功能特性

### 核心能力

- **多源 IP 聚合采集**：Cloudflare 官方 CIDR 列表 + 自定义 URL（支持多个来源），自动合并去重
- **多维度测速引擎**：TCP 延迟（握手）+ HTTP 响应 + 下载带宽，三维综合测速
- **智能 IP 筛选**：
  - 自动模式：按本机运营商线路自动匹配最优 IP
  - 手动模式：按用户指定国家代码精准筛选
  - 混合模式：50% 自动 + 50% 手动，兼顾覆盖与质量
- **两级归属地验证**：xdb 快速初筛 → cdn-cgi/trace 精准纠错 → 验证失败直接剔除
- **硬性准入规则**：TCP 延迟上限、丢包率上限、HTTP 延迟上限、下载带宽下限，任一不达标拒绝入库
- **增量测速机制**：SQLite 记忆历史结果，有效期内自动跳过，资源消耗降低 80%+
- **自适应重试**：批次降级重试（失败率 >30% 时自动降并发重试）
- **多目标推送**：Cloudflare DNS 记录 + GitHub 仓库文件 + WxPusher 微信通知
- **Web 全流程管理**：内置 Web Dashboard（go:embed 编译嵌入），在线配置、实时监控、一键操作
- **定时自动化**：每日定时采集 + 可配置间隔自动推送，后台守护进程模式
- **系统级资源清理**：GC 回收、临时文件清理、残留进程终止、数据库 VACUUM

### 评分体系（100 分制）

| 等级 | 分数范围 | 含义 |
|------|---------|------|
| S | ≥ 90 | 卓越（优先推送） |
| A | 80 - 89 | 优秀 |
| B | 60 - 79 | 良好 |
| C | 40 - 59 | 可用 |
| D | < 40 | 淘汰 |

默认权重：延迟 35% + 丢包率 25% + 带宽 30% + HTTP 抖动 10%（可配置）

### IP.txt 输出格式

```
IP地址:端口#国家代码 评分等级 带宽Mbps 延迟ms
```

示例：

```
108.162.193.209:2083#US A 44.00 Mbps 118.00 ms
111.97.164.62:8443#JP B 45.94 Mbps 119.00 ms
```

---

## 技术架构

### 系统工作流

```
IP 采集 → xdb 初筛 → 国家筛选 → trace 纠错验证 → 测速 → 评分 → 入库 → 推送
   │         │          │              │          │       │      │       │
   ▼         ▼          ▼              ▼          ▼       ▼      ▼       ▼
 官方+     毫秒级    手动/自动     精准验证    三维     硬性格   SQLite  CF DNS
 自定义    快速过滤   国家匹配     纠正误判    并发测速  入规则          GitHub
 合并去重                                      自适应重试          WxPusher
```

### 模块架构

```
┌─────────────────────────────────────────────────────────┐
│                    Web Dashboard                         │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐          │
│  │ 概览 │ │ 数据库│ │ 配置 │ │ 任务 │ │ 日志 │          │
│  └──────┘ └──────┘ └──────┘ └──────┘ └──────┘          │
└──────────────────────────┬──────────────────────────────┘
                           │ HTTP API
┌──────────────────────────┼──────────────────────────────┐
│                   业务服务层                              │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐      │
│  │  采集服务    │  │  测速服务    │  │  推送服务    │      │
│  └─────────────┘  └─────────────┘  └─────────────┘      │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────┼──────────────────────────────┐
│                    核心引擎层                             │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐          │
│  │TCP   │ │HTTP  │ │下载  │ │评分  │ │纠错  │          │
│  │测速  │ │测速  │ │测速  │ │引擎  │ │验证  │          │
│  └──────┘ └──────┘ └──────┘ └──────┘ └──────┘          │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────┼──────────────────────────────┐
│                    数据存储层                             │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐                  │
│  │SQLite│ │ip2reg│ │geo_  │ │CIDR  │                  │
│  │数据库│ │ion.xdb│ │corr  │ │stats │                  │
│  └──────┘ └──────┘ └──────┘ └──────┘                  │
└─────────────────────────────────────────────────────────┘
```

---

## 环境要求

### 必需环境

| 组件 | 版本要求 | 说明 |
|------|---------|------|
| Go | ≥ 1.21 | 编译运行（推荐 1.25+） |
| SQLite | 内置（CGO-free） | 使用 modernc.org/sqlite 纯 Go 实现，无需安装 SQLite |
| ip2region.xdb | 随项目提供 | IP 归属地查询库（需与二进制同目录） |

### 操作系统

支持所有 Go 可编译的平台：

- **Linux**（推荐）：Ubuntu / Debian / CentOS / Arch 等
- **macOS**：Intel / Apple Silicon
- **Windows**：10 / 11 / Server
- **FreeBSD** / **OpenBSD**

### 网络

- 需能访问 `cloudflare.com`（拉取 IP 列表和测速）
- 需能访问 `cdn-cgi/trace` 和 `speed.cloudflare.com`（测速与验证）
- 推送功能需相应的 API Token（Cloudflare / GitHub）

---

## 安装步骤

### 方式一：从源码编译（推荐）

#### Linux / macOS

```bash
# 1. 克隆项目
git clone https://github.com/Romaluo/cf-speedtest.git
cd cf-ip

# 2. 下载依赖
go mod download

# 3. 编译
go build -o cf-speedtest .

# 4. 确认 ip2region.xdb 存在
ls ip2region.xdb

# 5. 复制配置文件
cp config.yaml.example config.yaml  # 或直接编辑 config.yaml
```

#### Windows

```powershell
# 1. 克隆项目
git clone https://github.com/Romaluo/cf-speedtest.git
cd cf-ip

# 2. 编译
go build -o cf-speedtest.exe .

# 3. 确认 ip2region.xdb 存在
dir ip2region.xdb
```

#### 交叉编译

```bash
# Linux ARM64（如树莓派）
GOOS=linux GOARCH=arm64 go build -o cf-speedtest-arm64 .

# Windows 64位
GOOS=windows GOARCH=amd64 go build -o cf-speedtest.exe .

# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -o cf-speedtest-macos .
```

### 方式二：直接下载预编译二进制

从 [Releases](https://github.com/Romaluo/cf-speedtest/releases) 页面下载对应平台的二进制文件，解压后与 `ip2region.xdb` 和 `config.yaml` 放在同一目录。

---

## 快速开始

### 三步启动

```bash
# 1. 编辑配置
vim config.yaml

# 2. 启动服务（Web 模式）
./cf-speedtest -config config.yaml

# 3. 浏览器访问
# 打开 http://localhost:8080 登录使用
```

### 最小配置示例

```yaml
# 启用官方 IP 源
ipv4_enabled: true
ipv4_count: 500

# 选择日本 IP
ip_select_mode: manual
ip_select_countries: [JP]

# 开启 Web Dashboard
web_enable: true
web_host: 0.0.0.0
web_port: 8080
web_username: admin
web_password: your-password
```

### 后台自动化

```bash
# 启动并立即进入后台模式
./cf-speedtest -config config.yaml -daemon

# 或通过 systemd 服务（推荐）
sudo systemctl enable --now cf-speedtest
```

---

## 配置指南

### 配置文件

配置文件为 YAML 格式，默认路径 `config.yaml`，可通过 `-config` 参数指定。

### 配置参数详解

#### IP 来源

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ipv4_url` | string | `https://www.cloudflare.com/ips-v4` | IPv4 CIDR 列表 URL |
| `ipv4_enabled` | bool | `true` | 是否启用官方 IP 源 |
| `ipv4_count` | int | `100` | 从官方 CIDR 随机采样数量，0 = 不限制 |
| `extra_ip_urls` | []string | - | 额外 IP 列表 URL（每行一个 IP），填写你的自定义采集网址 |
| `cidr_stats_path` | string | `cidr_stats.json` | CIDR 权重统计文件路径，启用动态权重采样 |

#### IP 选择

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ip_select_mode` | string | `auto` | `auto`（按运营商线路）/ `manual`（按国家）/ `hybrid`（50% 混合） |
| `ip_select_countries` | []string | - | 指定国家代码，如 `[JP, SG, KR, HK]` |

#### IP 归属地验证

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `trace_verify_enable` | bool | `false` | 启用 cdn-cgi/trace 精准纠错验证 |
| `trace_verify_concurrency` | int | `10` | 纠错验证并发数（独立于测速并发） |
| `trace_endpoint` | string | `https://www.cloudflare.com/cdn-cgi/trace` | trace 端点 URL |
| `trace_http_timeout` | duration | `8s` | trace HTTP 请求超时 |
| `trace_connect_timeout` | duration | `4s` | trace HTTP 连接超时 |
| `geo_corrections_path` | string | `geo_corrections.txt` | 纠错记录文件路径（持久化验证结果） |

#### 测速参数

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `concurrency` | int | `50` | 并发测速协程数 |
| `tcp_ping_count` | int | `4` | 每个 IP 的 TCP 测速次数 |
| `tcp_ping_ports` | []int | `[443]` | TCP 测速端口列表（支持多端口，推荐 2053/2083/2087/2096/8443） |
| `tcp_ping_timeout` | duration | `3s` | TCP 连接超时 |
| `http_target` | string | `https://www.cloudflare.com/cdn-cgi/trace` | HTTP 测速目标 URL |
| `http_count` | int | `3` | HTTP 测速次数 |
| `http_timeout` | duration | `5s` | HTTP 总超时 |
| `http_connect_timeout` | duration | `0` | HTTP 连接超时（0 = 使用 http_timeout） |
| `dl_target` | string | `https://speed.cloudflare.com/__down` | 下载测速目标 URL |
| `dl_timeout` | duration | `30s` | 下载总超时 |
| `dl_connect_timeout` | duration | `0` | 下载连接超时（0 = 使用 dl_timeout） |
| `dl_read_timeout` | duration | `0` | 下载读取超时（0 = 使用 dl_timeout） |
| `dl_size` | int | `5242880` | 下载字节数（5MB），用于计算带宽 |
| `max_ips` | int | `200` | 最大测试 IP 数量，0 = 不限制 |

#### 重试策略

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `retry_count` | int | `1` | 单节点测速失败重试次数 |
| `retry_batch_fallback` | bool | `true` | 批次降级重试（失败率 >30% 时自动降并发重试） |

#### 硬性准入规则

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `rule_max_tcp_latency` | int | `1000` | TCP 延迟上限（毫秒），超过则拒绝入库 |
| `rule_max_loss_rate` | float | `0.0` | 丢包率上限（0.0-1.0），超过则拒绝入库 |
| `rule_max_http_latency` | int | `2000` | HTTP 延迟上限（毫秒），超过则拒绝入库 |
| `rule_min_download_mbps` | float | `0.1` | 下载带宽下限（Mbps），低于则拒绝入库 |

#### 评分权重

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `weight_latency` | float64 | `0.35` | 延迟权重 |
| `weight_loss` | float64 | `0.25` | 丢包率权重 |
| `weight_bandwidth` | float64 | `0.30` | 带宽权重 |
| `weight_jitter` | float64 | `0.10` | HTTP 抖动权重 |
| `min_score_threshold` | float64 | `80` | 入库最低评分阈值（100 分制） |
| `max_db_size` | int | `2000` | 数据库最大 IP 数量（超限时末位淘汰） |

#### Cloudflare DNS 推送

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `cf_api_key` | string | - | Cloudflare API Token（需 Zone.DNS 编辑权限） |
| `cf_zone_id` | string | - | Cloudflare Zone ID |
| `cf_dns_name` | string | - | DNS 记录名称，如 `ip.yourdomain.com` |
| `cf_dns_ttl` | int | `300` | DNS TTL（秒） |
| `cf_dns_options` | string | `proxied=true` | DNS 选项 |
| `cf_push_count` | int | `0` | 推送 IP 数量，0 = 不推送 |

#### GitHub 推送

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `github_token` | string | - | GitHub Personal Access Token（需 repo 权限） |
| `github_repo` | string | - | 仓库路径，格式 `user/repo` |
| `github_file_path` | string | `IP.txt` | 仓库中文件路径 |
| `github_branch` | string | `main` | 分支名 |
| `github_push_count` | int | `0` | 推送 IP 数量，0 = 不推送 |

#### WxPusher 微信通知

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `wxpusher_enable` | bool | `false` | 启用 WxPusher 推送 |
| `wxpusher_app_token` | string | - | 应用 Token（wxpusher.zjiecode.com 后台获取） |
| `wxpusher_topic_ids` | []int | - | 话题 ID 列表 |
| `wxpusher_uids` | []string | - | 用户 UID 列表 |

#### IP 风险过滤

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ip_risk_filter_enable` | bool | `false` | DNS 推送前过滤高风险 IP |
| `ip_risk_score_threshold` | int | `70` | 风险分数阈值（>70 视为高风险） |
| `ip_risk_filter_timeout` | duration | `5s` | 风险查询超时 |

#### 自动化运行

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `daemon_mode` | bool | `false` | 后台守护进程模式（Web UI 切换会自动持久化） |
| `collect_time` | string | `05:00` | 每日定时采集时间（HH:MM） |
| `push_interval` | int | `4` | 自动推送间隔（小时），0 = 不自动推送 |
| `db_path` | string | `speedtest.db` | SQLite 数据库路径 |
| `ip_expire_time` | duration | `24h` | IP 结果有效期（期内跳过重测） |
| `data_retention` | int | `30` | 历史数据保留天数 |
| `top_n` | int | `20` | 输出/推送前 N 个最优结果 |
| `direct_mode_enable` | bool | `false` | 直连模式（禁用 HTTP/HTTPS 代理） |

#### 资源清理

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `cleanup.enable` | bool | `true` | 启用资源清理 |
| `cleanup.strategies.benchmark.gc` | bool | `true` | 测速任务后 GC 回收 |
| `cleanup.strategies.benchmark.temp_files` | bool | `true` | 测速任务后清理临时文件 |
| `cleanup.strategies.benchmark.processes` | bool | `true` | 测速任务后终止残留进程 |
| `cleanup.strategies.push.db_vacuum` | bool | `true` | 推送任务后数据库 VACUUM |
| `cleanup.verify_resources` | bool | `true` | 清理后验证资源回归基线 |
| `cleanup.memory_threshold` | float | `0.9` | 期望释放的额外内存比例 |

#### Web Dashboard

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `web_enable` | bool | `false` | 启用 Web 面板 |
| `web_host` | string | `0.0.0.0` | 监听地址 |
| `web_port` | int | `8080` | 监听端口 |
| `web_username` | string | `admin` | 登录用户名 |
| `web_password` | string | `admin` | 登录密码 |
| `web_session_ttl` | duration | `12h` | 会话有效期 |

### 示例配置

```yaml
# IP 来源
ipv4_url: https://www.cloudflare.com/ips-v4
ipv4_enabled: true
ipv4_count: 1000
extra_ip_urls:
  - 你的自定义采集网址1
  - 你的自定义采集网址2
cidr_stats_path: cidr_stats.json

# IP 选择
ip_select_mode: manual
ip_select_countries:
  - JP
  - SG
  - KR
  - HK
  - TW

# IP 归属地验证
trace_verify_enable: true
trace_verify_concurrency: 10
geo_corrections_path: geo_corrections.txt

# 测速参数
concurrency: 80
tcp_ping_count: 2
tcp_ping_ports: [443, 2053, 2083, 2087, 2096, 8443]
tcp_ping_timeout: 3s
http_timeout: 8s
http_connect_timeout: 5s
dl_timeout: 10s
dl_connect_timeout: 2s
dl_read_timeout: 10s

# 重试策略
retry_count: 0
retry_batch_fallback: false

# 硬性准入规则
rule_max_tcp_latency: 1000
rule_max_loss_rate: 0
rule_max_http_latency: 2000
rule_min_download_mbps: 0.1

# 评分权重
weight_latency: 0.35
weight_loss: 0.25
weight_bandwidth: 0.30
weight_jitter: 0.10
min_score_threshold: 80
max_db_size: 2000
top_n: 100

# Cloudflare 推送
cf_api_key: "your-cloudflare-api-token"
cf_zone_id: "your-zone-id"
cf_dns_name: "ip.yourdomain.com"
cf_dns_ttl: 300
cf_dns_options: proxied=false
cf_push_count: 10

# GitHub 推送
github_token: "ghp_your_github_token"
github_repo: "user/repo"
github_file_path: IP.txt
github_branch: main
github_push_count: 50

# 自动化
daemon_mode: true
collect_time: "05:00"
push_interval: 4
ip_expire_time: 24h
data_retention: 30

# Web Dashboard
web_enable: true
web_host: 0.0.0.0
web_port: 8080
web_username: admin
web_password: "your-password"
```

---

## 使用教程

### 1. Web Dashboard（推荐）

```bash
# 启动服务
./cf-speedtest -config config.yaml
```

浏览器访问 `http://localhost:8080`，登录后即可使用：

| 页面 | 功能说明 |
|------|---------|
| **概览** | 统计卡片（IP 总数、今日入库、推送状态）、推送器状态、运行任务、启动后台模式 |
| **IP 数据库** | 浏览所有测速结果，支持国家/端口/评分筛选、排序、批量导出 CSV、数据库备份/恢复 |
| **配置** | 在线编辑所有配置项，带 ℹ 提示气泡，端口和国家代码快捷选择 |
| **任务** | 实时查看测速/推送任务进度、日志、取消运行中的任务 |
| **日志** | 查看系统运行日志 |

**核心操作按钮**：

- **立即测速**：仅执行 IP 采集 → 测速 → 入库（不推送）
- **立即推送**：对数据库内 IP 重测 → 推送
- **终止任务**：中途停止正在运行的任务
- **重启系统**：一键重启服务（保留后台模式状态）
- **后台模式开关**：启动/停止定时任务（需二次确认）

### 2. 命令行模式

#### 单次采集入库（不推送）

```bash
./cf-speedtest -config config.yaml
```

拉取 IP → TCP 预筛选 → 测速评分 → 入库，**不执行推送**。

#### 后台守护进程

```bash
./cf-speedtest -config config.yaml -daemon
```

按 `collect_time` 每日定时采集（支持多时间点如 `06:00,12:00,18:00`，采集后自动推送），每 `push_interval` 小时自动推送。

#### 仅推送（不测速）

```bash
./cf-speedtest -config config.yaml -push_only
```

从数据库读取已有结果，重新生成 IP.txt 并推送，不执行测速。

#### 全量重测并推送

```bash
./cf-speedtest -config config.yaml -rerun_all
```

重测数据库中所有 IP 并推送，适用于网络环境变化后刷新数据。

### 3. 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `config.yaml` | 配置文件路径 |
| `-daemon` | `false` | 后台运行模式 |
| `-log` | - | 日志文件路径 |
| `-interval` | `60` | 定时任务间隔（分钟） |
| `-version` | `false` | 打印版本信息 |
| `-concurrency` | `0` | 覆盖配置中的并发数 |
| `-top_n` | `0` | 覆盖配置中的输出数量 |
| `-max_ips` | `0` | 覆盖配置中的最大 IP 数量 |
| `-ipv4_only` | `false` | 仅测试 IPv4 |
| `-ip_mode` | - | IP 选择模式：`auto` / `manual` |
| `-rerun_all` | `false` | 全量重测并推送 |
| `-push_only` | `false` | 仅执行推送 |

### 4. systemd 服务（Linux 推荐）

#### 用户级服务（支持后台模式持久化）

创建 `~/.config/systemd/user/cf-speedtest.service`：

```ini
[Unit]
Description=CF SpeedTest Service
After=network.target

[Service]
Type=simple
WorkingDirectory=/home/zhimin/cf-speedtest
ExecStart=/home/zhimin/cf-speedtest/cf-speedtest -config /home/zhimin/cf-speedtest/config.yaml
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
# 启用并启动
systemctl --user daemon-reload
systemctl --user enable cf-speedtest
systemctl --user start cf-speedtest

# 查看状态
systemctl --user status cf-speedtest

# 日志
journalctl --user -u cf-speedtest -f

# 允许用户服务开机自启（无需登录）
sudo loginctl enable-linger $USER
```

#### 系统级服务

创建 `/etc/systemd/system/cf-speedtest.service`：

```ini
[Unit]
Description=CF SpeedTest Service
After=network.target

[Service]
Type=simple
User=www
WorkingDirectory=/opt/cf-speedtest
ExecStart=/opt/cf-speedtest/cf-speedtest -config /opt/cf-speedtest/config.yaml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable cf-speedtest
sudo systemctl start cf-speedtest
```

### 5. Watchdog 自动恢复

```bash
# 后台运行 watchdog 监控
nohup bash watchdog.sh > watchdog.log 2>&1 &
```

---

## 项目结构

```
cf-speedtest/
├── main.go                    # 入口：CLI 参数解析、运行模式调度、daemon 循环
├── config/
│   └── config.go              # 配置结构体、默认值、YAML 加载/保存、国家列表
├── collector/
│   ├── ipfetcher.go           # IP 源拉取与合并、CIDR 权重采样
│   └── cidr_stats.go          # CIDR 段权重统计与持久化
├── engine/                    # 测速引擎
│   ├── benchmark.go           # 测速主流程编排
│   ├── tcpping.go             # TCP 延迟测速
│   ├── httping.go             # HTTP 响应测速
│   ├── download.go            # 下载带宽测速
│   ├── prefilter.go           # TCP 预筛选（快速过滤不可达 IP）
│   ├── country.go             # 按国家筛选
│   ├── risk.go                # IP 风险等级过滤
│   └── directmode.go          # 直连模式
├── scorer/
│   └── scorer.go              # 综合评分计算、质量等级判定
├── geo/
│   ├── resolver.go            # IP 归属地查询（ip2region.xdb）
│   ├── batch_verify.go        # 批量 trace 纠错验证
│   └── corrections.go         # 纠错记录持久化
├── model/
│   └── result.go              # 数据模型定义（Task、IPResult）
├── repository/
│   ├── sqlite.go              # SQLite 数据库操作（CRUD、增量查询）
│   └── query.go               # 查询封装
├── pusher/                    # 推送模块
│   ├── cloudflare.go          # Cloudflare DNS 推送
│   ├── github.go              # GitHub 文件推送
│   └── wxpusher.go            # WxPusher 微信通知推送
├── output/
│   └── exporter.go            # 结果导出（CSV、IP.txt）
├── cleanup/                   # 资源清理框架
│   ├── cleaner.go             # 清理编排（按任务类型差异化策略）
│   ├── files.go               # 临时文件清理
│   ├── monitor.go             # 资源监控（内存/CPU/进程）
│   └── process.go             # 残留进程终止
├── web/                       # Web 服务
│   ├── server.go              # HTTP 服务器、路由注册、依赖注入
│   ├── handlers.go            # API 处理器（配置、任务、推送等）
│   ├── service.go             # 业务逻辑（测速流程、推送流程、任务管理）
│   ├── daemon.go              # 后台模式控制器（切换、状态、持久化）
│   ├── auth.go                # 认证中间件、会话管理
│   ├── database_handler.go    # 数据库 API（查询、清空、备份、恢复）
│   ├── restart.go             # 服务重启（systemd / fork-exec）
│   └── static/
│       └── index.html         # 前端单页应用（go:embed 编译嵌入）
├── log/
│   └── log.go                 # 日志模块
├── config.yaml                # 运行配置
├── ip2region.xdb              # IP 归属地数据库
├── geo_corrections.txt        # 纠错记录持久化文件
├── cidr_stats.json            # CIDR 权重统计文件
├── speedtest.db               # SQLite 数据库（运行时生成）
├── go.mod / go.sum            # Go 模块定义
└── watchdog.sh                # 进程看门狗脚本
```

---

## 工作流程

### 手动测速流程（立即测速按钮）

```
1. 拉取 IP
   ├─ 官方 CIDR 列表（Cloudflare）
   └─ 自定义 URL 列表
        │
        ▼
2. 端口预过滤
   └─ 清除数据库中不在用户配置端口的旧记录
        │
        ▼
3. 增量筛选
   └─ 跳过有效期内已有结果的 IP（24h 内）
        │
        ▼
4. TCP 握手预筛选
   └─ 并发测试连通性，清除不可达 IP
        │
        ▼
5. xdb 国家初筛
   └─ ip2region.xdb 快速填充国家代码
        │
        ▼
6. 手动模式：国家筛选
   └─ 仅保留指定国家的 IP
        │
        ▼
7. trace 纠错验证（可选）
   ├─ 已在纠错层的 IP → 直接修正
   ├─ 需验证的 IP → cdn-cgi/trace 精准验证
   ├─ 验证成功 → 更新国家代码
   └─ 验证失败 → 从列表移除（不进入测速）
        │
        ▼
8. 三维测速
   ├─ TCP 延迟（多次测量取平均）
   ├─ HTTP 响应（多次测量取平均）
   └─ 下载带宽（5MB 文件下载）
        │
        ▼
9. 评分与入库
   ├─ 单条即时评分
   ├─ 硬性准入规则验证
   ├─ 评分阈值过滤
   └─ 批量写入 SQLite
        │
        ▼
10. 批次降级重试（可选）
    └─ 失败率 >30% 时降并发重试
        │
        ▼
11. 数据清理
    ├─ 末位淘汰（超过 max_db_size）
    └─ 过期清理（超过 data_retention）
        │
        ▼
    完成（不执行推送）
```

### 手动推送流程（立即推送按钮）

```
1. 读取数据库 IP
   └─ 按端口筛选所有历史 IP
        │
        ▼
2. TCP 握手预筛选
   └─ 快速过滤不可达 IP
        │
        ▼
3. 全量重测
   ├─ TCP + HTTP + 下载带宽
   └─ 自适应批次降级重试
        │
        ▼
4. 评分与入库更新
   └─ 新结果覆盖旧结果
        │
        ▼
5. 生成 IP.txt
   └─ 按评分排序，取前 top_n
        │
        ▼
6. 多目标推送
   ├─ Cloudflare DNS（cf_push_count 条）
   ├─ GitHub 仓库（github_push_count 条）
   └─ WxPusher 通知（可选）
        │
        ▼
    完成
```

---

## 常见问题（FAQ）

### Q: 首次运行没有测速结果？

**A:** 检查以下几点：
1. 网络能否访问 `cloudflare.com` 和 `speed.cloudflare.com`
2. `ipv4_enabled` 是否为 `true`
3. `tcp_ping_ports` 是否至少配置了一个端口
4. `min_score_threshold` 是否过高（建议初始设为 40 进行测试）
5. 硬性准入规则是否过严（`rule_max_loss_rate: 0` 表示任何丢包都拒绝）
6. 查看日志 `cf-speedtest.log` 中的错误信息

### Q: 推送失败怎么办？

**A:** 常见原因：
- **Cloudflare 推送**：检查 `cf_api_key`、`cf_zone_id`、`cf_dns_name` 是否正确，Token 需有 Zone.DNS 编辑权限，`cf_push_count > 0` 才会推送
- **GitHub 推送**：检查 `github_token` 是否有效，需有仓库写入权限（`repo` 权限），`github_push_count > 0` 才会推送
- **WxPusher 推送**：确认 `wxpusher_enable: true` 和 App Token 正确
- **网络问题**：确认服务器能访问 `api.cloudflare.com` 和 `api.github.com`

### Q: 增量测速是什么机制？

**A:** 系统将每次测速结果存入 SQLite 数据库，并记录 `updated_at` 时间戳。下次采集时，若 IP 的测速结果在 `ip_expire_time`（默认 24h）内，则跳过重测。仅对新出现的 IP 和已过期的 IP 执行测速，大幅降低资源消耗。

### Q: 硬性准入规则与评分阈值有什么区别？

**A:** 硬性准入规则是"一票否决"——任一条件不满足直接拒绝入库，不参与评分。评分阈值是"综合判定"——通过所有硬性规则后，按加权评分判定是否入库。建议：硬性规则设宽松（避免误杀），评分阈值设严格（保证质量）。

### Q: 数据库会无限增长吗？

**A:** 不会。通过三重机制控制：
- `max_db_size`：超过上限时自动末位淘汰最低分 IP
- `data_retention`：自动清理超过保留天数的旧数据
- `ip_expire_time`：过期 IP 在下次采集时被标记为重测对象

### Q: trace 纠错验证的作用是什么？

**A:** 解决 ip2region.xdb 对 Cloudflare 动态 IP 归属地识别不准的问题。xdb 是离线静态数据库，对新分配的 IP 段可能误判。通过 cdn-cgi/trace 端点实时查询，可获取准确的国家代码。验证结果持久化到 `geo_corrections.txt`，下次无需重复验证。

### Q: CIDR 权重采样是怎么工作的？

**A:** 系统会在每次测速后统计每个 CIDR 段通过筛选的 IP 数量，保存到 `cidr_stats.json`。下次采集时，优先从通过率高的 CIDR 段采样，提高优质 IP 命中率。首次使用时采用均匀采样。

### Q: Web 面板忘记密码怎么办？

**A:** 直接编辑 `config.yaml` 中的 `web_password` 字段，重启服务即可。

### Q: 如何切换 IP 选择模式？

**A:** 在 Web 配置页面切换：
- **自动模式**：系统根据本机运营商线路自动选择最优 IP，无需指定国家
- **手动模式**：仅保留指定国家的 IP（通过国家代码选择器配置）
- **混合模式**：推送时 50% 来自指定国家，50% 来自自动选择

### Q: 支持自建 IP 源吗？

**A:** 支持。在 `extra_ip_urls` 中添加 URL，每行一个 IP 地址即可。系统会自动合并所有来源的 IP 并去重。

### Q: 如何在后台长期运行？

**A:** 推荐三种方式（按优先级）：
1. **systemd 用户级服务**（推荐）：支持后台模式状态持久化，重启后自动恢复
2. **守护进程模式**：`./cf-speedtest -daemon`
3. **watchdog 脚本**：`nohup bash watchdog.sh &`

### Q: 后台模式切换后重启会丢失吗？

**A:** 不会。通过 Web UI 切换后台模式时，`daemon_mode` 会自动持久化到 `config.yaml`。系统重启后读取配置自动恢复定时运行状态。

### Q: 为什么"立即测速"不推送？

**A:** 设计上"立即测速"仅负责采集入库，"立即推送"负责重测推送。这样可以：
- 在网络好时批量采集入库
- 在需要时随时推送，不受采集时间限制
- 避免每次测速都触发推送 API 调用

### Q: 批次降级重试是什么？

**A:** 当单次测速失败率超过 30% 时，系统自动将并发数减半进行二次重试。适用于网络不稳定的场景，用更长时间换取更高成功率。可通过 `retry_batch_fallback: false` 关闭。

---

## 故障排除

### 查看日志

```bash
# 实时查看日志
tail -f cf-speedtest.log

# 查看错误日志
grep "ERROR" cf-speedtest.log

# 查看最近的推送日志
grep "推送" cf-speedtest.log | tail -20

# 查看 daemon 相关日志
grep "DAEMON" cf-speedtest.log

# 查看纠错验证日志
grep "纠错" cf-speedtest.log
```

### 常见错误

#### `database is locked`

SQLite 并发写入冲突。系统已内置 `SetMaxOpenConns(1)` 串行化访问，正常情况不会出现。若仍发生，等待几秒后重试。

#### `TCP 连接超时`

- 检查防火墙是否放行测速端口（443, 8443, 2053 等）
- 检查服务器到 Cloudflare 的网络连通性
- 尝试降低 `concurrency` 并发数
- 确认 `tcp_ping_ports` 中的端口在 Cloudflare 支持

#### `下载测速失败`

- 确认 `dl_target` URL 可访问
- 检查 `dl_timeout` 和 `dl_read_timeout` 是否过短
- 确认 `dl_size` 设置合理（建议 5MB = 5242880）

#### `Web 面板无法访问`

- 确认 `web_enable: true`
- 检查端口是否被占用：`netstat -tlnp | grep 8080`
- 检查防火墙是否放行 `web_port`
- systemd 环境检查：`systemctl --user status cf-speedtest`

#### `ip2region.xdb not found`

确认文件存在于程序同目录，或在 `config.yaml` 中通过 `ip_db_path` 指定完整路径。

#### `daemon_mode 重启后丢失`

确认已使用最新版本（v1.0.0+），通过 Web UI 切换后台模式会自动持久化。

### 性能调优

| 场景 | 建议配置 |
|------|---------|
| 快速测试 | `concurrency: 100`, `tcp_ping_count: 1`, `http_count: 1`, `dl_size: 1048576`, `min_score_threshold: 40` |
| 精确测试 | `concurrency: 30`, `tcp_ping_count: 4`, `http_count: 5`, `dl_size: 10485760`, `min_score_threshold: 90` |
| 低配服务器 | `concurrency: 20`, `max_ips: 200`, `max_db_size: 500`, `retry_batch_fallback: false` |
| 大规模采集 | `concurrency: 100`, `ipv4_count: 5000`, `max_ips: 4000`, `max_db_size: 2000`, `top_n: 200` |
| 日本 IP 专项 | `ip_select_mode: manual`, `ip_select_countries: [JP]`, `trace_verify_enable: true` |

---

## 贡献规范

欢迎提交 Issue 和 Pull Request。

### 开发环境

```bash
# 克隆并编译
git clone https://github.com/Romaluo/cf-speedtest.git
cd cf-ip
go mod download
go build -o cf-speedtest .

# 运行测试
go test ./...
```

### 代码规范

- 遵循 [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- 使用 `gofmt` 格式化代码
- 新增功能需有合理注释
- 不引入 CGO 依赖（保持纯 Go 交叉编译能力）
- Web 前端修改后需重新编译（`//go:embed static/*` 编译嵌入）

### 提交规范

```
<type>: <description>

[optional body]
```

Type 可选：`feat`（新功能）、`fix`（修复）、`docs`（文档）、`refactor`（重构）、`perf`（性能）、`chore`（杂项）

### 分支策略

- `main`：稳定版本
- 开发请从 `main` 拉取分支，PR 合并回 `main`

---

## 开源许可

本项目采用 [MIT License](LICENSE) 开源协议。

---

**免责声明**：本工具仅供学习和个人使用。使用本工具产生的任何后果（包括但不限于 IP 被封、服务异常）由使用者自行承担。请遵守 Cloudflare 和 GitHub 的服务条款。
