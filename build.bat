@echo off
rem KunPay Windows 本机编译脚本(编译 Windows 版本)
cd /d %~dp0
set CGO_ENABLED=0
echo ==^> 编译 windows/amd64
if not exist dist mkdir dist
go build -trimpath -ldflags "-s -w" -o dist\kunpay_windows_amd64\kunpay.exe ./cmd/server
echo ==^> 完成: dist\kunpay_windows_amd64\kunpay.exe
pause
