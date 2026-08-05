#!/bin/bash
# setup-mock-update.sh — 构建本地 mock 更新服务器,用于测试 cf-speedtest 自动更新流程
#
# 用法:
#   ./scripts/setup-mock-update.sh              # 默认端口 18080
#   ./scripts/setup-mock-update.sh 19000         # 自定义端口
#   ./scripts/setup-mock-update.sh 19000 1.3.0   # 自定义端口 + 版本号
#
# 前置条件:
#   - Go 工具链 (go 1.21+)
#   - python3 (用于启动 HTTP 服务器)
#   - 当前位于 cf-speedtest 项目根目录
#
# 工作流程:
#   1. 构建当前 cf-speedtest 二进制(作为"新版本"模拟)
#   2. 打包为 tar.gz (含 cf-speedtest + ip2region.xdb + config.yaml.example)
#   3. 计算 sha256 + size
#   4. 生成 mock version.json (版本号默认 1.2.0,高于当前 1.1.0)
#   5. 启动本地 HTTP 服务器提供 version.json + tar.gz
#   6. 打印配置说明
#
# 测试方法:
#   1. 修改 config.yaml: update_check_url: "http://localhost:18080/version.json"
#   2. 启动 cf-speedtest (./cf-speedtest -daemon)
#   3. 通过 Web UI 或 API 触发更新: curl -X POST http://localhost:8080/api/update/apply
#   4. 观察日志: tail -f cf-speedtest.log | grep UPDATE
#   5. 进度通过 SSE 查看: curl -N http://localhost:8080/api/update/progress

set -e

# ===== 参数解析 =====
MOCK_PORT="${1:-18080}"
MOCK_VERSION="${2:-1.2.0}"

# ===== 路径常量 =====
PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MOCK_DIR="/tmp/cf-speedtest-mock"
GO_BIN="${GO_BIN:-/usr/local/go/bin/go}"

# ===== 颜色输出 =====
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
step()  { echo -e "${CYAN}[STEP]${NC} $1"; }

# ===== 前置检查 =====
if ! command -v "$GO_BIN" &>/dev/null; then
    # 尝试 PATH 中的 go
    if command -v go &>/dev/null; then
        GO_BIN="go"
    else
        echo "错误: 未找到 Go 工具链 (尝试设置 GO_BIN 环境变量)"
        exit 1
    fi
fi

if ! command -v python3 &>/dev/null; then
    echo "错误: 未找到 python3 (用于启动 HTTP 服务器)"
    echo "安装: sudo apt install python3  或  sudo yum install python3"
    exit 1
fi

# ===== Step 0: 清理旧文件 =====
step "0/6 清理旧的 mock 文件..."
rm -rf "$MOCK_DIR"
mkdir -p "$MOCK_DIR"

# ===== Step 1: 构建当前二进制(作为"新版本") =====
step "1/6 构建新版本二进制 (v$MOCK_VERSION)..."
info "从 $PROJECT_ROOT 构建..."
cd "$PROJECT_ROOT"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -ldflags "-s -w" -o "$MOCK_DIR/cf-speedtest" main.go
info "构建完成: $MOCK_DIR/cf-speedtest ($(stat -c%s "$MOCK_DIR/cf-speedtest") 字节)"

# ===== Step 2: 复制资源文件 =====
step "2/6 复制资源文件..."
for f in ip2region.xdb config.yaml.example; do
    if [ -f "$PROJECT_ROOT/$f" ]; then
        cp "$PROJECT_ROOT/$f" "$MOCK_DIR/"
        info "已复制: $f"
    else
        warn "未找到 $f, 跳过(更新流程仍可测试,但不会替换该文件)"
    fi
done

# ===== Step 3: 打包 tar.gz =====
step "3/6 打包 tar.gz..."
ARCHIVE_NAME="cf-speedtest-linux-amd64.tar.gz"
ARCHIVE_PATH="$MOCK_DIR/$ARCHIVE_NAME"

# 构建 tar 包内容列表(仅包含存在的文件)
TAR_FILES="cf-speedtest"
for f in ip2region.xdb config.yaml.example; do
    if [ -f "$MOCK_DIR/$f" ]; then
        TAR_FILES="$TAR_FILES $f"
    fi
done

tar -czf "$ARCHIVE_PATH" -C "$MOCK_DIR" $TAR_FILES
info "打包完成: $ARCHIVE_NAME ($(stat -c%s "$ARCHIVE_PATH") 字节)"

# ===== Step 4: 计算 sha256 + size =====
step "4/6 计算 sha256 + size..."
SHA256=$(sha256sum "$ARCHIVE_PATH" | awk '{print $1}')
SIZE=$(stat -c%s "$ARCHIVE_PATH")
info "sha256: $SHA256"
info "size:   $SIZE 字节"

# ===== Step 5: 生成 mock version.json =====
step "5/6 生成 mock version.json..."
PUBLISHED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# 生成 Windows 资产条目(如果存在 windows 二进制)
WIN_ARCHIVE="cf-speedtest-windows-amd64.zip"
if [ -f "$MOCK_DIR/$WIN_ARCHIVE" ]; then
    WIN_SHA256=$(sha256sum "$MOCK_DIR/$WIN_ARCHIVE" | awk '{print $1}')
    WIN_SIZE=$(stat -c%s "$MOCK_DIR/$WIN_ARCHIVE")
    WIN_ASSET=$(cat <<EOF
    ,
    "windows/amd64": {
      "url": "http://localhost:$MOCK_PORT/$WIN_ARCHIVE",
      "size": $WIN_SIZE,
      "sha256": "$WIN_SHA256"
    }
EOF
)
else
    WIN_ASSET=""
fi

cat > "$MOCK_DIR/version.json" <<EOF
{
  "version": "$MOCK_VERSION",
  "release_notes": "## 测试版本 v$MOCK_VERSION\n\n这是一个 mock 版本,用于测试 cf-speedtest 的自动更新流程。\n\n### 测试内容\n- 版本检查 (Checker)\n- 下载 (Downloader, 断点续传)\n- SHA256 校验 (Verifier)\n- 解压 (Installer.Extract)\n- 替换二进制 (Installer.Install)\n- 重启 (Restarter)\n\n### 注意\n此版本的实际二进制与 v1.1.0 相同,仅 version.json 中的版本号不同。\n更新完成后,二进制仍报告 v1.1.0,但更新流程日志可验证整个流程是否正常。",
  "published_at": "$PUBLISHED_AT",
  "min_required_version": "1.0.0",
  "assets": {
    "linux/amd64": {
      "url": "http://localhost:$MOCK_PORT/$ARCHIVE_NAME",
      "size": $SIZE,
      "sha256": "$SHA256"
    }$WIN_ASSET
  }
}
EOF

info "version.json 已生成: $MOCK_DIR/version.json"

# ===== Step 6: 启动 HTTP 服务器 =====
step "6/6 启动 mock HTTP 服务器 (端口 $MOCK_PORT)..."

echo ""
echo "============================================================"
echo "  Mock 更新服务器已就绪"
echo "============================================================"
echo ""
echo -e "${CYAN}服务地址:${NC}"
echo "  version.json:  http://localhost:$MOCK_PORT/version.json"
echo "  下载包:        http://localhost:$MOCK_PORT/$ARCHIVE_NAME"
echo ""
echo -e "${CYAN}配置方法 (修改 config.yaml):${NC}"
echo "  update_check_enable: true"
echo "  update_check_url: \"http://localhost:$MOCK_PORT/version.json\""
echo "  update_check_interval: 1m   # 测试时缩短为 1 分钟"
echo ""
echo -e "${CYAN}测试命令:${NC}"
echo ""
echo "  # 1. 启动 cf-speedtest (使用修改后的 config.yaml)"
echo "  ./cf-speedtest -daemon"
echo ""
echo "  # 2. 手动触发版本检查"
echo "  curl -X POST http://localhost:8080/api/update/check"
echo ""
echo "  # 3. 查看更新状态"
echo "  curl http://localhost:8080/api/update/status"
echo ""
echo "  # 4. 触发一键更新"
echo "  curl -X POST http://localhost:8080/api/update/apply"
echo ""
echo "  # 5. 实时查看更新进度 (SSE)"
echo "  curl -N http://localhost:8080/api/update/progress"
echo ""
echo "  # 6. 查看更新日志"
echo "  grep UPDATE cf-speedtest.log | tail -50"
echo ""
echo -e "${YELLOW}注意:${NC}"
echo "  - mock 二进制与当前 v1.1.0 相同,仅 version.json 声明为 v$MOCK_VERSION"
echo "  - 更新完成后进程会重启,重启后仍报告 v1.1.0(因二进制未变)"
echo "  - 临时文件在 $MOCK_DIR/,可手动清理"
echo "  - 按 Ctrl+C 停止 mock 服务器"
echo "============================================================"
echo ""

cd "$MOCK_DIR"
exec python3 -m http.server "$MOCK_PORT" --bind 127.0.0.1
