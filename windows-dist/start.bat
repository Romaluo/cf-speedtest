@echo off
chcp 65001 >nul
title cf-speedtest Cloudflare IP 测速工具

REM ============================================
REM cf-speedtest Windows 启动脚本
REM 用法:
REM   start.bat              - 以 Web Dashboard 模式启动(前台)
REM   start.bat daemon       - 以后台守护模式启动
REM   start.bat benchmark    - 执行单次测速后退出
REM   start.bat push         - 仅执行推送
REM ============================================

cd /d "%~dp0"

REM 检查可执行文件是否存在
if not exist "cf-speedtest.exe" (
    echo [ERROR] 未找到 cf-speedtest.exe
    echo 请确认文件位于同一目录: %~dp0
    pause
    exit /b 1
)

REM 检查 IP 数据库
if not exist "ip2region.xdb" (
    echo [WARN] 未找到 ip2region.xdb,IP 归属地解析将以降级模式运行
)

REM 首次运行时若 config.yaml 不存在,则从示例复制
if not exist "config.yaml" (
    if exist "config.yaml.example" (
        echo [INFO] 首次运行,从 config.yaml.example 创建 config.yaml
        copy "config.yaml.example" "config.yaml" >nul
        echo [INFO] 请编辑 config.yaml 修改用户名/密码/推送配置后重新运行
        echo.
        notepad "config.yaml"
        pause
        exit /b 0
    )
)

REM 根据参数选择运行模式
set MODE=%1
if "%MODE%"=="" set MODE=web

if /i "%MODE%"=="web" (
    echo [INFO] 以 Web Dashboard 模式启动...
    echo [INFO] 启动后请访问 http://127.0.0.1:8080
    echo [INFO] 按 Ctrl+C 可停止服务
    echo.
    cf-speedtest.exe -config config.yaml
) else if /i "%MODE%"=="daemon" (
    echo [INFO] 以守护模式启动...
    cf-speedtest.exe -config config.yaml -daemon
) else if /i "%MODE%"=="benchmark" (
    echo [INFO] 执行单次测速...
    cf-speedtest.exe -config config.yaml -interval 0
) else if /i "%MODE%"=="push" (
    echo [INFO] 仅执行推送...
    cf-speedtest.exe -config config.yaml -push_only
) else (
    echo [ERROR] 未知模式: %MODE%
    echo 用法: start.bat [web^|daemon^|benchmark^|push]
    exit /b 1
)

pause
