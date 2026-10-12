@echo off
rem KunPay 监控助手 Windows 服务一键安装(需管理员权限,依赖 NSSM)
rem 用法: 把 KunPayMonitor_win64.exe 与本脚本放同一目录,右键"以管理员身份运行"
rem NSSM 下载: https://nssm.cc/download (nssm.exe 放入同目录或 PATH)

set APP_DIR=%~dp0
set SVC_NAME=KunPayMonitor

nssm install %SVC_NAME% "%APP_DIR%KunPayMonitor_win64.exe"
nssm set %SVC_NAME% AppDirectory "%APP_DIR%"
nssm set %SVC_NAME% AppParameters "-port 8210 -no-open"
nssm set %SVC_NAME% DisplayName "KunPay Monitor 监控助手"
nssm set %SVC_NAME% Description "KunPay 码支付云端挂机监控服务(心跳保活+到账上报)"
nssm set %SVC_NAME% Start SERVICE_AUTO_START
nssm start %SVC_NAME%

echo.
echo 服务已安装并启动。控制台地址: http://127.0.0.1:8210
echo 卸载: nssm stop %SVC_NAME% ^&^& nssm remove %SVC_NAME% confirm
pause
