#!/usr/bin/env bash
# KunPay Monitor 安卓云手机一键初始化
# 用法: ./init-monitor.sh <adb端口> <平台地址> <设备Token>
#   示例: ./init-monitor.sh 15555 "http://pay.example.com" "你的设备Token"
# 依赖: 本机已装 adb(platform-tools)与 scrcpy(可选,登录账号投屏用)
set -e

PORT="${1:?用法: $0 <adb端口> <平台地址> <设备Token>}"
SERVER="${2:?缺少平台地址}"
TOKEN="${3:?缺少设备Token}"
ADB="${ADB:-$HOME/.android-sdk/platform-tools/adb}"
APK="${APK:-$(dirname "$0")/../../monitor-android/KunPayMonitor.apk}"
COMPONENT="com.kunpay.monitor/.MonitorService"
SERIAL="127.0.0.1:$PORT"

echo "==> 连接云手机 $SERIAL"
adb connect "$SERIAL"
adb -s "$SERIAL" wait-for-device

echo "==> 安装 KunPay Monitor"
adb -s "$SERIAL" install -r "$APK"

echo "==> 授予通知权限与监听权限"
adb -s "$SERIAL" shell pm grant com.kunpay.monitor android.permission.POST_NOTIFICATIONS
adb -s "$SERIAL" shell cmd notification allow_listener "$COMPONENT"

echo "==> 启动 APP 并自动填写配置"
adb -s "$SERIAL" shell am start -n com.kunpay.monitor/.MainActivity
sleep 3
# 320x640 小屏坐标: 平台地址框(160,318) TAB 切到 Token 框 TAB 切到保存按钮回车提交
adb -s "$SERIAL" shell input tap 160 318; sleep 1
adb -s "$SERIAL" shell input text "$SERVER"; sleep 1
adb -s "$SERIAL" shell input keyevent 61; sleep 0.5
adb -s "$SERIAL" shell input text "$TOKEN"; sleep 1
adb -s "$SERIAL" shell input keyevent 61; sleep 0.5
adb -s "$SERIAL" shell input keyevent 66; sleep 3

echo "==> 验证心跳(平台设备列表应显示在线)"
echo "完成。登录微信/支付宝账号请用 scrcpy 投屏: scrcpy -s $SERIAL"
