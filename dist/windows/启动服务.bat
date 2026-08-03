@echo off
chcp 65001 >nul
title CF-SpeedTest 服务
echo ============================================
echo   CF-SpeedTest Cloudflare IP 测速优选
echo ============================================
echo.
echo 正在启动 Web Dashboard...
echo 浏览器访问: http://localhost:8080
echo 按 Ctrl+C 停止服务
echo.
cf-speedtest-windows-amd64.exe -config config.yaml
pause
