@echo off
chcp 65001 >nul
title CF-SpeedTest 后台守护
echo ============================================
echo   CF-SpeedTest 后台守护模式
echo ============================================
echo.
echo 服务将按配置自动采集和推送
echo 按 Ctrl+C 停止
echo.
start "" /b cf-speedtest-windows-amd64.exe -config config.yaml -daemon
echo 已启动后台守护进程
timeout /t 5 >nul
