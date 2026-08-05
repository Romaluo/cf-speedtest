# cf-speedtest 自动版本检测与更新功能 — 实施方案

## 一、需求摘要

为客户端实现自动版本检测与一键更新:
- **检查频率**:启动时检查一次 + 每 24h 定时检查(可配置)
- **版本源**:仓库内 `version.json` (通过 `raw.githubusercontent.com` 访问)
- **更新策略**:一键自动安装(下载 → 校验 → 替换 → 重启)
- **平台**:Linux(systemd)+ Windows(.bat 启动)
- **安全**:sha256 校验、断点续传、HTTPS、降级保护

## 二、架构设计

### 数据流

```
[启动] → 启动后台 goroutine(updateChecker)
              ↓
        等待 30s(避免与初始化竞争)
              ↓
        拉取 version.json → 解析 → 对比当前 version
              ↓
        有新版本? → 缓存到内存(updateInfo)
              ↓
        ticker 每 24h 重复
              ↓
[Web UI] 轮询 /api/update/status → 显示更新提醒
              ↓
用户点击"立即更新" → POST /api/update/apply
              ↓
[updateManager] 下载 → 校验 → 解压 → 替换 → 重启
              ↓
进度通过 /api/update/progress (SSE) 推送到 Web UI
``### 组件分层

```
main.go              ← 启动 updateChecker goroutine
config/config.go     ← 新增 update_* 配置项
updater/
  ├── checker.go     ← 版本检查器(拉取/解析/对比 version.json)
  ├── downloader.go  ← 下载器(断点续传、进度回调)
  ├── installer.go   ← 安装器(解压、替换、平台分支)
  └── manager.go     ← 更新管理器(状态机、协调各步骤)
web/
  ├── update_handler.go  ← HTTP handlers (新增)
  ├── server.go          ← 注册新路由
  └── static/index.html  ← 更新提醒 UI + 进度条
version.json         ← 仓库根的版本元数据(随代码提交)
```

## 三、version.json 格式

放在仓库根,通过 `https://raw.githubusercontent.com/Romaluo/cf-speedtest/main/version.json` 访问。

```json
{
  "version": "1.2.0",
  "release_notes": "新增自动更新功能;修复 X 个 bug",
  "published_at": "2026-09-01T10:00:00Z",
  "min_required_version": "1.1.0",
  "assets": {
    "linux/amd64": {
      "url": "https://github.com/Romaluo/cf-speedtest/releases/download/v1.2.0/cf-speedtest-linux-amd64.tar.gz",
      "size": 9371134,
      "sha256": "74798af8e7ef28c5fa188d38f74138c73d7a7ead82a210eb8a478759f748b2a8"
    },
    "windows/amd64": {
      "url": "https://github.com/Romaluo/cf-speedtest/releases/download/v1.2.0/cf-speedtest-windows-amd64.zip",
      "size": 9169360,
      "sha256": "162931c6786c8e5edba6853a1dda432e3de98c560546f80757412466dc3fa324"
    }
  }
}
```

字段说明:
- `version`:最新版本号(语义化)
- `release_notes`:更新内容(纯文本,Web UI 展示)
- `published_at`:发布时间(用于显示)
- `min_required_version`:最低兼容版本(低于此版本不允许更新,需要先升级到中间版本)
- `assets`:按 `GOOS/GOARCH` 索引的下载信息

## 四、配置项(config.yaml)

```yaml
# ===== 自动更新 =====
update_check_enable: true          # 是否启用更新检查
update_check_url: "https://raw.githubusercontent.com/Romaluo/cf-speedtest/main/version.json"
update_check_interval: 24h         # 检查间隔
update_auto_download: false        # 检测到新版本是否自动下载(不自动安装)
update_temp_dir: ""                # 临时目录(空=系统默认 /tmp 或 %TEMP%)
```

## 五、API 设计

### 1. `GET /api/update/status`(需鉴权)
查询当前更新状态。

**响应**:
```json
{
  "current_version": "1.1.0",
  "latest_version": "1.2.0",
  "has_update": true,
  "release_notes": "新增自动更新功能...",
  "published_at": "2026-09-01T10:00:00Z",
  "update_size": 9371134,
  "last_check_at": "2026-08-04T22:30:00Z",
  "last_check_error": "",
  "state": "idle"  // idle | checking | downloading | verifying | installing | restarting | failed
}
```

### 2. `POST /api/update/check`(需鉴权)
手动触发版本检查。

**响应**:同 `/api/update/status`

### 3. `POST /api/update/apply`(需鉴权)
触发一键更新流程。立即返回 202 Accepted,后台异步执行。

**响应**:
```json
{ "ok": true, "message": "更新流程已启动" }
```

### 4. `GET /api/update/progress`(需鉴权,SSE)
SSE 推送更新进度。

**事件流**:
```
event: progress
data: {"state":"downloading","percent":45,"speed":"256 KB/s","eta":"00:02:30"}

event: progress
data: {"state":"verifying","percent":100}

event: progress
data: {"state":"installing","percent":50,"message":"替换二进制文件..."}

event: done
data: {"state":"restarting","message":"更新完成,服务重启中..."}

event: error
data: {"state":"failed","error":"sha256 校验失败"}
```

### 5. `POST /api/update/cancel`(需鉴权)
取消正在进行的下载(仅 downloading 状态可取消)。

## 六、更新流程详解

### 状态机

```
idle → checking → downloading → verifying → extracting → replacing → restarting → done
                ↓ (失败)           ↓            ↓            ↓
                failed             failed       failed       failed
```

### 详细步骤

**Step 1: 下载(downloading)**
- 目标文件:`{temp_dir}/cf-speedtest-update-{version}.{ext}`
- HTTP Range 请求实现断点续传:
  - 如果临时文件已存在,发送 `Range: bytes={size}-`
  - 服务器返回 206 → 追加写入
  - 服务器返回 200 → 从头写入
  - 失败重试时,从已下载大小续传(最多 3 次)
- 进度回调:每 500ms 推送一次 SSE 事件(下载字节数 / 总大小 / 速度 / ETA)
- 用户取消:关闭 response body,中断下载

**Step 2: 校验(verifying)**
- 计算 sha256(下载文件)
- 对比 version.json 里的 `assets[platform].sha256`
- 不匹配 → 删除文件 → 返回错误
- 同时校验文件大小(防止下载不完整)

**Step 3: 解压(extracting)**
- Linux tar.gz:用 `archive/tar` + `compress/gzip` 标准库解压
  - 解压到 `{temp_dir}/cf-speedtest-{version}/`
  - 提取 `cf-speedtest` 二进制 + `ip2region.xdb` + `config.yaml.example`
- Windows zip:用 `archive/zip` 标准库解压
  - 解压到 `{temp_dir}/cf-speedtest-{version}/`
  - 提取 `cf-speedtest-windows-amd64.exe` + `ip2region.xdb`

**Step 4: 替换(replacing)** — 平台分支

**Linux**:
```go
// 1. 找到当前二进制路径
exe, _ := os.Executable()
// 2. 写入新二进制到临时文件(同目录)
tmpExe := exe + ".new"
writeFile(tmpExe, newBinary, 0755)
// 3. 原子 rename (Linux 支持 rename 覆盖运行中文件)
os.Rename(tmpExe, exe)
// 4. ip2region.xdb 替换(如果新版本带了)
os.Rename(newXdb, filepath.Join(dir, "ip2region.xdb"))
// 5. config.yaml.example 替换(可选,不覆盖 config.yaml)
```

**Windows**(运行中 .exe 不能写,但能 rename):
```go
exe, _ := os.Executable()
// 1. 先 rename 当前 exe 为 .old (Windows 特性,允许重命名运行中文件)
os.Rename(exe, exe + ".old")
// 2. 写入新 exe
writeFile(exe, newBinary, 0755)
// 3. ip2region.xdb 替换(运行中文件能写,直接覆盖)
os.Rename(newXdb, "ip2region.xdb")
// 4. 标记"启动后删除 .old"(写入 update_state.json)
// 5. 启动新进程(继承 stdio)
// 6. 旧进程退出
// 7. 新进程启动时检查并删除 .old 文件
```

**Step 5: 重启(restarting)**
- 复用 `web/restart.go` 的逻辑:
  - systemd 环境:调 `restartViaSystemd()` (但需要先做替换)
  - 非 systemd:调 `restartViaForkExec()`
- **关键调整**:restart.go 的现有逻辑是"重启当前进程",更新流程是"替换文件后重启"
- 在 installer.go 完成替换后,直接调用 srv 的 restart 方法(需注入到 manager)

**Step 6: 启动后清理**
- 新进程启动时,检查 `update_state.json`(或数据库):
  - 如果有"待删除的 .old 文件" → 删除
  - 如果有"上次更新成功"标记 → 写日志"更新到 vX.Y.Z 成功"
- Windows 删除 `.old` 在新进程启动时执行

## 七、Web UI 改动

### 1. 顶部更新提醒 banner
- 当 `GET /api/update/status` 返回 `has_update: true` 时,顶部显示:
  ```
  📢 发现新版本 v1.2.0 | 大小: 8.9 MB | 更新内容: 新增自动更新功能...
  [立即更新] [稍后提醒] [查看详情]
  ```

### 2. 更新进度模态框
- 用户点击"立即更新"后,弹出模态框:
  - 进度条(百分比)
  - 当前步骤(下载中 / 校验中 / 安装中 / 重启中)
  - 速度 + ETA(下载阶段)
  - 日志区(滚动显示关键步骤)
  - 取消按钮(仅下载阶段可用)
- SSE 连接 `/api/update/progress` 实时更新

### 3. 设置页 — 更新配置
- 在现有设置页增加"自动更新"区块:
  - 启用/禁用更新检查
  - 检查间隔(下拉:12h / 24h / 7d)
  - 自动下载开关
  - "立即检查更新"按钮
  - "上次检查时间"显示

## 八、安全考虑

1. **传输安全**:HTTPS 下载(GitHub Releases 强制 HTTPS)
2. **完整性校验**:sha256(VERSION.json 提供 + GitHub Release API 也有 digest 字段)
3. **降级保护**:对比 `min_required_version`,低于则拒绝更新
4. **临时文件权限**:`0600`(下载中)→ `0755`(二进制)
5. **文件覆盖原子性**:
   - Linux: `os.Rename` 原子操作
   - Windows: rename-then-write(利用 Windows 允许重命名运行中 exe 的特性)
6. **失败回滚**:
   - 替换前备份当前二进制到 `.bak`
   - 校验失败、解压失败 → 删除临时文件,不影响运行中服务
   - 替换失败(罕见)→ 恢复 `.bak`
7. **认证**:所有更新 API 都需 session 鉴权(防止未授权触发更新)
8. **并发控制**:同一时间只允许一个更新任务(updateManager 用 mutex 保护)

## 九、断点续传实现要点

```go
// 伪代码
func downloadWithResume(url, destPath string, expectedSize int64) error {
    // 1. 检查已下载大小
    var startByte int64
    if info, err := os.Stat(destPath); err == nil {
        startByte = info.Size()
    }
    
    // 2. 构建 Range 请求
    req, _ := http.NewRequest("GET", url, nil)
    if startByte > 0 {
        req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startByte))
    }
    
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return err  // 重试时从 startByte 继续
    }
    defer resp.Body.Close()
    
    // 3. 判断是否续传
    if resp.StatusCode == http.StatusPartialContent {
        // 续传:追加写入
        f, _ := os.OpenFile(destPath, os.O_APPEND|os.O_WRONLY, 0600)
        defer f.Close()
        io.Copy(f, resp.Body)
    } else if resp.StatusCode == http.StatusOK {
        // 服务器不支持续传或文件已改变:从头写入
        f, _ := os.Create(destPath)
        defer f.Close()
        io.Copy(f, resp.Body)
    }
    
    // 4. 校验最终大小
    if info, _ := os.Stat(destPath); info.Size() != expectedSize {
        return errors.New("下载大小不匹配")
    }
    return nil
}
```

## 十、文件改动清单

### 新增文件
1. `updater/checker.go` — 版本检查器(拉取/解析/对比)
2. `updater/downloader.go` — 下载器(断点续传、进度)
3. `updater/installer.go` — 安装器(平台分支替换)
4. `updater/manager.go` — 更新管理器(状态机、协调)
5. `updater/types.go` — 数据结构定义
6. `web/update_handler.go` — HTTP handlers
7. `version.json` — 仓库根版本元数据

### 修改文件
1. `main.go` — 启动 updateChecker goroutine
2. `config/config.go` — 新增 `Update*` 配置项
3. `config.yaml.example` — 添加配置示例
4. `dist/windows/config.yaml` — 同步配置项
5. `web/server.go` — 注册新路由
6. `web/restart.go` — 抽取重启逻辑供 manager 调用(可选,直接复用也可)
7. `web/static/index.html` — 添加更新提醒 + 进度 UI
8. `.gitignore` — 忽略 `update_state.json`、`*.old`、`*.bak`

## 十一、实施步骤(分阶段)

### 阶段 1:版本检查(可独立验证)
1. 创建 `version.json`(当前 v1.1.0 内容)
2. 实现 `updater/checker.go` — 拉取 + 解析 + 对比
3. 实现 `web/update_handler.go` — `/api/update/status` + `/api/update/check`
4. 在 `main.go` 启动后台检查 goroutine
5. 新增配置项

### 阶段 2:下载 + 校验
6. 实现 `updater/downloader.go` — 断点续传 + sha256 校验
7. 实现 `/api/update/apply` 触发下载
8. 实现 `/api/update/progress` SSE 推送进度
9. Web UI 添加进度模态框

### 阶段 3:安装 + 重启
10. 实现 `updater/installer.go` — 解压 + 平台替换
11. 整合 `web/restart.go` 完成重启
12. Windows `.old` 清理逻辑

### 阶段 4:Web UI + 完善
13. 顶部 banner 提醒
14. 设置页更新配置
15. 异常处理 + 重试
16. 文档更新(README 加自动更新说明)

## 十二、测试要点

- ✅ 版本对比(相同/新版本/旧版本/格式错误)
- ✅ 网络失败重试(模拟断网)
- ✅ 断点续传(下载到一半中断,重连续传)
- ✅ sha256 校验失败(故意修改下载文件)
- ✅ Linux systemd 重启(模拟 INVOCATION_ID)
- ✅ Linux 非 systemd fork-exec
- ✅ Windows exe 替换(rename .old → write → restart → delete .old)
- ✅ 并发触发(多次点击"立即更新")
- ✅ 取消下载
- ✅ 降级保护(尝试更新到更低版本)
- ✅ 配置禁用检查(`update_check_enable: false`)

## 十三、不在本需求范围

- 差分更新(只下载变化部分)— 过度工程,暂不做
- 灰度发布 — 个人项目不需要
- 强制更新 — 用户始终可选择"稍后提醒"
- 多架构支持(arm64/darwin)— 当前只支持 linux/amd64 和 windows/amd64
- 自签名验签 — 用 GitHub Release 的 sha256 已足够

## 十四、风险与缓解

| 风险 | 缓解 |
|---|---|
| 国内访问 raw.githubusercontent.com 慢 | 允许配置 `update_check_url` 指向镜像(如 ghproxy) |
| Windows exe 被占用无法替换 | rename-then-write 策略 + 启动后清理 .old |
| 更新中断导致服务损坏 | 备份 .bak + 失败回滚 + 仅替换二进制(不动 config.yaml) |
| 用户在更新过程中操作 | UI 禁用其他按钮 + API 返回 409 Conflict |
| 临时文件占用磁盘 | 更新完成/失败后清理 |
