# cf-speedtest Windows 环境使用说明

> Cloudflare IP 测速工具 · Windows 桌面版部署与使用指南

---

## 目录

1. [项目简介](#1-项目简介)
2. [系统环境要求](#2-系统环境要求)
3. [文件清单与存放路径](#3-文件清单与存放路径)
4. [快速启动](#4-快速启动)
5. [启动步骤详解](#5-启动步骤详解)
6. [功能模块说明](#6-功能模块说明)
7. [配置参数详解](#7-配置参数详解)
8. [Web Dashboard 使用](#8-web-dashboard-使用)
9. [常见问题排查](#9-常见问题排查)
10. [卸载与清理](#10-卸载与清理)

---

## 1. 项目简介

cf-speedtest 是一款 Cloudflare CDN IP 测速工具,能够:

- **批量拉取** Cloudflare 官方 IP 段及自定义 IP 列表
- **多维度测速**:TCP 延迟、丢包率、HTTP 响应延迟、下载速度
- **智能评分**:综合延迟、丢包、带宽、抖动加权打分
- **地理筛选**:按国家/地区手动筛选,或按本机运营商自动匹配最优归属
- **DNS 自动推送**:将优选 IP 推送到 Cloudflare DNS / GitHub 仓库
- **Web 管理面板**:可视化查看测速结果、手动触发任务、数据库管理
- **守护进程模式**:无人值守定时测速并推送

Windows 版为单文件绿色版,**无需安装 Go 环境、无需任何运行时依赖**,下载解压即可运行。

---

## 2. 系统环境要求

### 2.1 最低系统要求

| 项目 | 要求 |
|---|---|
| 操作系统 | Windows 10 (64位) 及以上,兼容 Windows 11 |
| 架构 | x86-64 (Intel/AMD 64位 CPU) |
| 内存 | 512 MB 以上(测速时建议 1 GB) |
| 磁盘空间 | 50 MB(程序)+ 数据库预留 100 MB |
| 网络 | 需访问互联网(测速目标为 Cloudflare 节点) |

### 2.2 软件依赖

- **无需安装** Go 语言环境
- **无需安装** Visual C++ 运行库
- **无需安装** Python / Node.js 等任何运行时
- 可执行文件已静态链接所有依赖,**纯绿色版**

### 2.3 网络要求

- 出站访问 `www.cloudflare.com`(拉取 IP 段)
- 出站访问 `speed.cloudflare.com`(测速目标)
- 出站访问 `ipinfo.io` / 自定义 IP 信息 API(可选)
- 出站访问 `api.cloudflare.com` / `api.github.com`(推送功能,可选)
- 入站访问本机 `8080` 端口(Web Dashboard,可选)

> 若处于公司内网,请确保上述域名未被代理拦截。如需通过代理访问,请在 `config.yaml` 中保持 `direct_mode_enable: false`,系统会自动读取 `HTTP_PROXY` 环境变量。

### 2.4 Windows 防火墙提示

首次运行时,Windows 可能弹出"Windows 安全中心警报"询问是否允许 `cf-speedtest.exe` 通过防火墙。请选择:

- **专用网络**:建议勾选(局域网访问 Web Dashboard 时需要)
- **公用网络**:不建议勾选(公网环境存在安全风险)

如仅在本机访问 Web Dashboard,可全部取消勾选。

---

## 3. 文件清单与存放路径

### 3.1 分发包内容

将分发包(压缩包)解压到任意目录,例如 `D:\cf-speedtest\`。解压后目录结构如下:

```
D:\cf-speedtest\
├── cf-speedtest.exe          # 主程序(必须)
├── ip2region.xdb             # IP 归属地数据库(必须)
├── config.yaml.example       # 配置文件示例(首次运行时自动复制为 config.yaml)
├── start.bat                 # 启动脚本(双击运行)
└── README-Windows.md         # 本使用说明
```

### 3.2 运行时自动生成的文件

首次运行后,程序会在同目录生成以下文件:

```
D:\cf-speedtest\
├── config.yaml               # 实际使用的配置文件(从 example 复制或自动生成)
├── speedtest.db              # SQLite 测速结果数据库
├── cf-speedtest.log          # 运行日志
├── IP.txt                    # 优选 IP 列表(导出文件)
├── result.csv                # CSV 格式测速结果(从 Web 导出时生成)
└── backups\                  # 数据库自动备份目录
    └── backup_YYYYMMDD_HHMMSS.db
```

### 3.3 推荐存放路径

| 场景 | 推荐路径 |
|---|---|
| 个人单用户使用 | `D:\cf-speedtest\` 或 `C:\cf-speedtest\` |
| 多用户共享 | `C:\ProgramData\cf-speedtest\` |
| 桌面快捷启动 | 在桌面创建 `start.bat` 的快捷方式 |

> ⚠️ **不要**将程序放在 `C:\Program Files\` 下,该目录写入需要管理员权限,会导致数据库和日志无法创建。

---

## 4. 快速启动

**3 分钟快速上手**(适合不想看长篇文档的用户):

1. 将分发包解压到 `D:\cf-speedtest\`
2. 双击 `start.bat`
3. 首次运行会自动打开 `config.yaml`,修改 `web_username` 和 `web_password` 后保存关闭
4. 再次双击 `start.bat`
5. 浏览器访问 `http://127.0.0.1:8080`,用刚才设置的用户名密码登录
6. 在 Web 界面点击"开始测速"即可

---

## 5. 启动步骤详解

### 5.1 方式一:双击 start.bat 启动(推荐)

`start.bat` 提供四种运行模式,通过参数选择:

| 命令 | 说明 |
|---|---|
| 双击 `start.bat` | 默认 Web 模式,前台运行,关闭窗口即停止 |
| `start.bat daemon` | 守护模式,后台定时测速并推送 |
| `start.bat benchmark` | 单次测速模式,测完即退出 |
| `start.bat push` | 仅执行推送(不测速) |

**首次启动流程**:

1. 双击 `start.bat`
2. 脚本检测到无 `config.yaml`,自动从 `config.yaml.example` 复制
3. 自动用记事本打开 `config.yaml`,请修改以下关键项:
   - `web_username`:Web 登录用户名
   - `web_password`:Web 登录密码(**请务必修改默认值**)
4. 保存并关闭记事本
5. 按任意键关闭窗口
6. 再次双击 `start.bat` 正式启动

### 5.2 方式二:命令行启动

按 `Win+R`,输入 `cmd` 打开命令提示符:

```cmd
D:
cd \cf-speedtest

REM Web 模式启动
cf-speedtest.exe -config config.yaml

REM 守护模式启动(后台定时任务)
cf-speedtest.exe -config config.yaml -daemon

REM 单次测速后退出
cf-speedtest.exe -config config.yaml -interval 0

REM 仅推送
cf-speedtest.exe -config config.yaml -push_only

REM 全量重测并推送
cf-speedtest.exe -config config.yaml -rerun_all

REM 自定义并发数
cf-speedtest.exe -config config.yaml -concurrency 100

REM 查看版本
cf-speedtest.exe -version
```

### 5.3 方式三:开机自启(守护模式)

如需开机自动后台运行:

1. 按 `Win+R`,输入 `shell:startup` 打开启动文件夹
2. 在该文件夹新建快捷方式,指向 `D:\cf-speedtest\start.bat`
3. 右键快捷方式 → 属性 → 在"目标"末尾添加 ` daemon`
4. 修改"运行方式"为"最小化"
5. 完成。下次开机将自动以守护模式启动

### 5.4 命令行参数完整列表

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-config` | `config.yaml` | 配置文件路径 |
| `-daemon` | `false` | 后台守护模式(定时执行) |
| `-log` | (空) | 日志文件路径,空则用配置文件中的 `log_file` |
| `-interval` | `60` | 定时任务间隔(分钟),`0` 表示仅执行一次 |
| `-version` | `false` | 打印版本信息并退出 |
| `-concurrency` | `0` | 并发数,`0` 表示用配置文件中的值 |
| `-top_n` | `0` | 输出前 N 个结果 |
| `-max_ips` | `0` | 最大测试 IP 数量 |
| `-ipv4_only` | `false` | 仅测试 IPv4 |
| `-ip_mode` | (空) | IP 选择模式:`auto` / `manual` |
| `-rerun_all` | `false` | 全量重测数据库中所有 IP 并推送 |
| `-push_only` | `false` | 仅执行推送,不测速 |

---

## 6. 功能模块说明

### 6.1 IP 拉取模块(collector)

- 从 Cloudflare 官方 API 拉取 IPv4 段(`https://www.cloudflare.com/ips-v4`)
- 支持额外的自定义 IP 列表 URL(`extra_ip_urls`)
- CIDR 段自动展开并随机采样(每段默认采样 10 个 IP)
- 支持 `auto` / `manual` / `hybrid` 三种选择模式

### 6.2 测速引擎(engine)

| 测速项 | 说明 | 配置项 |
|---|---|---|
| TCP Ping | 测量 TCP 握手延迟和丢包率 | `tcp_ping_count`, `tcp_ping_ports`, `tcp_ping_timeout` |
| HTTP Ping | 测量 HTTP 请求响应延迟 | `http_count`, `http_timeout`, `http_target` |
| 下载测速 | 测量从 Cloudflare 下载速度 | `dl_target`, `dl_timeout`, `dl_size` |
| 预过滤 | 快速 TCP 检测过滤不可达 IP | 自动启用 |

测速采用并发流水线,默认并发 50,可在配置中调整 `concurrency`。

### 6.3 评分模块(scorer)

综合得分 = 延迟分 × `weight_latency` + 丢包分 × `weight_loss` + 带宽分 × `weight_bandwidth` + 抖动分 × `weight_jitter`

权重总和应为 `1.0`,默认配置侧重延迟和带宽。

### 6.4 IP 入库硬规则

以下任一条件不满足,IP **不会入库**(即使测速成功):

| 配置项 | 默认值 | 含义 |
|---|---|---|
| `rule_max_tcp_latency` | `1000` | TCP 延迟上限(ms) |
| `rule_max_loss_rate` | `0` | 丢包率上限(0~1) |
| `rule_max_http_latency` | `2000` | HTTP 延迟上限(ms) |
| `rule_min_download_mbps` | `0.1` | 下载速度下限(Mbps) |

### 6.5 地理解析模块(geo)

- 使用 ip2region 离线数据库(`ip2region.xdb`)查询 IP 归属地
- 自动检测本机公网 IP 和所属运营商
- `auto` 模式下自动筛选与运营商匹配的归属地 IP

### 6.6 推送模块(pusher)

| 推送通道 | 说明 | 配置前缀 |
|---|---|---|
| Cloudflare DNS | 通过 API 更新 DNS 记录,自动切换到最优 IP | `cf_*` |
| GitHub | 将 IP 列表推送到 GitHub 仓库文件 | `github_*` |
| WxPusher | 微信推送测速完成通知 | `wxpusher_*` |

所有推送均为可选,无需推送的通道留空或设 `count=0` 即可。

### 6.7 Web Dashboard(web)

- 仪表盘:实时任务进度、最新统计
- IP 数据库:可排序、可筛选、可导出 CSV
- 任务管理:手动触发测速/推送/重测
- 系统设置:在线修改配置
- 数据库管理:备份、恢复、清理、压缩

详见 [第 8 节](#8-web-dashboard-使用)。

### 6.8 资源清理模块(cleanup)

- 测速后自动清理临时文件
- 检测并终止卡死的测速子进程
- 数据库超阈值自动清理旧数据
- 内存超阈值自动触发 GC

---

## 7. 配置参数详解

配置文件为 `config.yaml`(YAML 格式)。所有参数均可通过文本编辑器修改,修改后需重启程序生效。

### 7.1 IP 来源

```yaml
ipv4_url: https://www.cloudflare.com/ips-v4   # Cloudflare 官方 IP 段 API
ipv4_enabled: true                            # 是否启用 IPv4 拉取
ipv4_count: 1000                              # 从官方段采样的 IP 数量上限
extra_ip_urls:                                # 额外 IP 列表 URL
    - https://your-custom-ip-list-1.txt
    - https://your-custom-ip-list-2.txt
```

### 7.2 IP 选择模式

```yaml
ip_select_mode: auto        # auto: 按本机运营商自动匹配
                            # manual: 按 ip_select_countries 国家列表筛选
                            # hybrid: 各取 50%
ip_select_countries:        # manual/hybrid 模式下的国家代码列表
    - JP                    # 日本
    - SG                    # 新加坡
    - KR                    # 韩国
    - HK                    # 香港
    - TW                    # 台湾
    - US                    # 美国
```

### 7.3 测速参数

```yaml
concurrency: 50              # 并发测速数,Windows 建议不超过 100
tcp_ping_count: 4            # 每个 IP 端口的 TCP ping 次数
tcp_ping_ports:              # 测试的端口列表
    - 443
    - 2053
    - 2083
    - 2087
    - 2096
    - 8443
tcp_ping_timeout: 3s         # TCP ping 超时
http_target: https://speed.cloudflare.com/__down
http_count: 3                # HTTP ping 次数
http_timeout: 5s
dl_target: https://speed.cloudflare.com/__down
dl_timeout: 8s
dl_size: 5242880             # 下载测速字节数(5MB)
max_ips: 2000                # 单次测速 IP 总数上限
```

### 7.4 评分权重

```yaml
weight_latency: 0.35         # 延迟权重
weight_loss: 0.25            # 丢包率权重
weight_bandwidth: 0.3        # 带宽权重
weight_jitter: 0.1           # 抖动权重
# 四项总和应为 1.0
```

### 7.5 数据库与输出

```yaml
min_score_threshold: 50      # 最低入库分数
max_db_size: 1000            # 数据库最大记录数,超出后自动清理低分旧数据
top_n: 100                   # 输出/推送的 IP 数量
db_path: speedtest.db        # SQLite 数据库文件路径
ip_db_path: ip2region.xdb    # IP 归属地数据库路径
log_file: cf-speedtest.log   # 日志文件路径
```

### 7.6 推送配置

**Cloudflare DNS 推送**:

```yaml
cf_api_key: "your_api_key"        # Cloudflare API Token
cf_zone_id: "your_zone_id"        # 域名所在 Zone ID
cf_dns_name: "cf.yourdomain.com"  # 要更新的 DNS 记录名
cf_dns_ttl: 300                   # DNS TTL(秒)
cf_dns_options: proxied=false     # 选项,proxied=true 表示走 CF 代理
cf_push_count: 10                 # 推送 IP 数量(轮询)
```

**GitHub 推送**:

```yaml
github_token: "ghp_xxxxx"              # GitHub Personal Access Token
github_repo: "username/repo"           # 仓库(需有写权限)
github_file_path: "IP.txt"             # 推送到的文件路径
github_branch: "main"                  # 分支
github_push_count: 50                  # 推送 IP 数量
```

**WxPusher 微信通知**:

```yaml
wxpusher_enable: false
wxpusher_app_token: "your_app_token"
wxpusher_topic_ids: []
wxpusher_uids:
    - "UID_xxxx"
```

### 7.7 运行模式

```yaml
daemon_mode: false           # true 为守护模式,程序不退出定时执行
interval: 60                 # 守护模式测速间隔(分钟)
collect_time: "06:00"        # 定时执行的具体时刻(24h 制,优先级高于 interval)
push_interval: 6             # 推送间隔(测速次数,每 N 次测速推送一次)
data_retention: 30           # 数据保留天数,超过自动清理
ip_expire_time: 24h0m0s      # IP 数据过期时间,过期后重测
```

### 7.8 Web Dashboard

```yaml
web_enable: true
web_host: 0.0.0.0            # 监听地址,0.0.0.0 表示所有网卡
                              # 仅本机访问可改为 127.0.0.1
web_port: 8080                # 端口
web_username: admin           # 登录用户名
web_password: admin           # 登录密码(请务必修改!)
web_session_ttl: 12h0m0s      # 会话有效期
```

### 7.9 高级选项

```yaml
direct_mode_enable: false     # 直连模式,启用后强制禁用系统代理
ip_risk_filter_enable: false  # IP 风险等级过滤
ip_risk_score_threshold: 70   # 风险分阈值(0-100,越低越宽松)
availability_check_enable: false  # 第三方可用性检测
retry_count: 1                # 失败重试次数
retry_batch_fallback: true    # 重试失败后回退到批量重测
```

### 7.10 资源清理

```yaml
cleanup:
    enable: true
    strategies:
        benchmark:            # 测速后清理策略
            gc: true          # 触发 Go GC
            temp_files: true  # 清理临时文件
            processes: true   # 终止卡死进程
            db_vacuum: false  # 数据库压缩(耗时,默认关闭)
            verify: true      # 验证资源完整性
        push:                 # 推送后清理策略
            gc: true
            temp_files: true
            processes: true
            db_vacuum: true   # 推送后压缩(空闲时执行)
            verify: true
    process_timeout: 5s       # 进程清理超时
    verify_resources: true    # 全局资源验证开关
    memory_threshold: 0.9     # 内存占用阈值(0-1)
```

---

## 8. Web Dashboard 使用

### 8.1 访问方式

启动程序后,在浏览器打开:

- 本机访问:`http://127.0.0.1:8080`
- 局域网访问:`http://<本机IP>:8080`(需 `web_host: 0.0.0.0` 且防火墙放行)

### 8.2 登录

使用 `config.yaml` 中配置的 `web_username` / `web_password` 登录。默认 `admin/admin`,**请尽快修改**。

### 8.3 主要功能页面

| 页面 | 功能 |
|---|---|
| 仪表盘 | 显示最近任务进度、IP 总数、最优 IP 摘要 |
| IP 数据库 | 查询、排序、筛选测速结果,支持 CSV 导出 |
| 任务管理 | 手动触发测速、推送、全量重测;查看历史任务 |
| 系统设置 | 在线编辑配置文件、切换守护模式、重启服务 |
| 数据库管理 | 备份/恢复/清理/压缩数据库,查看存储占用 |

### 8.4 手动触发测速

1. 进入"任务管理"页面
2. 点击"开始测速"按钮
3. 在"仪表盘"或"任务管理"页面查看实时进度
4. 测速完成后,结果自动入库,可在"IP 数据库"页面查看

### 8.5 守护模式切换

在"系统设置"页面可在线切换:

- **关闭守护**:测完一次后程序进入空闲
- **开启守护**:按 `interval` 或 `collect_time` 定时执行
- 切换后无需重启程序,立即生效

---

## 9. 常见问题排查

### 9.1 启动失败

**问题:双击 start.bat 闪退**

- 在 `start.bat` 所在目录打开 cmd,运行 `cf-speedtest.exe -config config.yaml` 查看错误信息
- 检查 `config.yaml` 是否存在且格式正确(YAML 缩进必须用空格,不能用 Tab)
- 检查 `ip2region.xdb` 是否在同目录

**问题:提示"端口被占用"**

```yaml
# 修改 config.yaml 中的端口
web_port: 8081   # 改为其他端口
```

或停止占用 8080 端口的其他程序:

```cmd
netstat -ano | findstr :8080
taskkill /PID <PID> /F
```

**问题:Windows 防火墙拦截**

控制面板 → Windows Defender 防火墙 → 允许应用通过防火墙 → 找到 `cf-speedtest.exe` → 勾选专用网络。

### 9.2 测速问题

**问题:测速数量很少 / 全部失败**

- 检查网络是否能访问 `https://www.cloudflare.com`
- 检查是否被代理软件干扰,尝试在 `config.yaml` 中启用 `direct_mode_enable: true`
- 适当降低 `concurrency`(如改为 20),避免系统连接数限制
- 查看 `cf-speedtest.log` 中具体错误

**问题:测速很慢**

- 降低 `tcp_ping_count`(如 2)和 `http_count`(如 2)
- 降低 `dl_size`(如改为 1MB = `1048576`)
- 降低 `max_ips`(如 500)
- 提高 `tcp_ping_timeout` 容忍更多慢速 IP

**问题:测速结果为 0**

- 确认本机能访问 `https://speed.cloudflare.com/__down`
- 公司网络可能拦截 Cloudflare,尝试切换网络或开代理(同时关闭 `direct_mode_enable`)

### 9.3 推送问题

**问题:Cloudflare DNS 推送失败**

- 确认 `cf_api_key` 是有效的 API Token(非 Global API Key)
- 确认 Token 有 `Zone.DNS` 编辑权限
- 确认 `cf_zone_id` 正确(在 Cloudflare 控制台首页右下角)
- 确认 `cf_dns_name` 已存在为 DNS A 记录(程序是更新,非创建)

**问题:GitHub 推送失败**

- 确认 `github_token` 有效且未过期
- 确认 Token 有 `repo` 权限
- 确认对 `github_repo` 指定的仓库有写权限
- 确认 `github_branch` 分支存在

### 9.4 Web Dashboard 问题

**问题:登录后立即跳回登录页**

- 检查系统时间是否正确,会话依赖时间戳
- 尝试清除浏览器 Cookie 后重新登录
- 适当延长 `web_session_ttl`(如 `24h0m0s`)

**问题:页面显示但数据不更新**

- 浏览器强制刷新:`Ctrl+F5`
- 检查任务是否正在运行(任务管理页面)
- 查看 `cf-speedtest.log` 是否有错误

### 9.5 数据库问题

**问题:数据库文件越来越大**

- 在 Web Dashboard "数据库管理"页面执行"压缩"(VACUUM)
- 降低 `max_db_size`(如 500)
- 降低 `data_retention`(如 7 天)

**问题:数据库损坏**

- 停止程序
- 在 Web Dashboard "数据库管理"页面从备份恢复
- 或删除 `speedtest.db` 重新开始(会丢失历史数据)

### 9.6 Windows 特定问题

**问题:被杀毒软件误报**

cf-speedtest 是纯 Go 编译的程序,无恶意代码。如被误报:

- 在杀毒软件中将 `cf-speedtest.exe` 添加到信任区/白名单
- Windows Defender:设置 → 病毒和威胁防护 → 排除项 → 添加 `cf-speedtest.exe`

**问题:守护模式下进程消失**

Windows 不支持 Unix 的 fork 守护进程。本程序的"守护模式"实际是前台运行,关闭窗口即停止。如需后台运行:

- 使用"任务计划程序"创建任务,触发器设为"启动时",操作指向 `cf-speedtest.exe`
- 或参考 [5.3 节](#53-方式三开机自启守护模式) 通过启动文件夹开机自启

**问题:CPU 使用率监控为 0**

CPU 使用率监控依赖 Linux 的 `/proc/stat`,在 Windows 上不可用,显示为 0 是正常现象,不影响其他功能。

**问题:无法终止卡死的子进程**

进程清理依赖 `SIGTERM`/`SIGKILL` 信号,Windows 对此支持有限。如遇测速卡死:

- 直接关闭程序窗口
- 任务管理器结束 `cf-speedtest.exe` 进程
- 重启程序

### 9.7 日志查看

日志文件:`cf-speedtest.log`(与 exe 同目录)

查看最近 50 行日志(cmd 中):

```cmd
powershell -c "Get-Content cf-speedtest.log -Tail 50"
```

---

## 10. 卸载与清理

### 10.1 完整卸载

1. 关闭程序(关闭窗口或在任务管理器结束 `cf-speedtest.exe`)
2. 删除程序所在目录(如 `D:\cf-speedtest\`)
3. 如创建了开机自启快捷方式,在启动文件夹(`shell:startup`)中删除
4. 如在防火墙添加了规则,在防火墙设置中移除

### 10.2 仅清理数据(保留程序)

删除以下文件即可重置数据:

- `speedtest.db` - 测速数据库
- `cf-speedtest.log` - 日志
- `IP.txt` / `result.csv` - 导出文件
- `backups\` 目录 - 数据库备份

> 删除前建议先在 Web Dashboard 中导出 CSV 备份重要数据。

---

## 附录:版本信息

- 程序版本:见 `cf-speedtest.exe -version` 输出
- 编译目标:`windows/amd64`(x86-64)
- 最低 Windows 版本:Windows 10 1809 / Windows Server 2019
- Go 运行时:静态链接,无外部依赖

---

*如遇本文档未涵盖的问题,请查看程序日志 `cf-speedtest.log` 中的错误信息。*
