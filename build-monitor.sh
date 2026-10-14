#!/usr/bin/env bash
# KunPay 监控助手(桌面端)跨平台编译
# 产物: dist/monitor/ 下的 Windows / macOS / Linux 单文件二进制
set -e
cd "$(dirname "$0")"

export CGO_ENABLED=0
mkdir -p dist/monitor
LDFLAGS="-s -w"

echo "==> 编译 KunPay 监控助手"
build() {
  local os=$1 arch=$2 out=$3
  echo "    $os/$arch"
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/monitor/$out" ./cmd/monitor-desktop
}

build windows amd64 "KunPayMonitor_win64.exe"
build windows arm64 "KunPayMonitor_win_arm64.exe"
build darwin  arm64 "KunPayMonitor_mac"
build darwin  amd64 "KunPayMonitor_mac_intel"

# macOS: 合并为 Universal 二进制(一台 Mac 通用),打包 .app 双击可开,并做 ad-hoc 签名
if [ "$(uname)" = "Darwin" ] && [ -x "dist/monitor/KunPayMonitor_mac" ]; then
  lipo -create -output dist/monitor/KunPayMonitor_universal \
    dist/monitor/KunPayMonitor_mac dist/monitor/KunPayMonitor_mac_intel 2>/dev/null \
    && echo "==> macOS Universal 二进制已合并"
  APP="dist/monitor/KunPayMonitor.app/Contents/MacOS"
  mkdir -p "$APP"
  if [ -f dist/monitor/KunPayMonitor_universal ]; then
    cp dist/monitor/KunPayMonitor_universal "$APP/KunPayMonitor"
  else
    cp dist/monitor/KunPayMonitor_mac "$APP/KunPayMonitor"
  fi
  chmod +x "$APP/KunPayMonitor"
  cat > dist/monitor/KunPayMonitor.app/Contents/Info.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>KunPayMonitor</string>
  <key>CFBundleDisplayName</key><string>KunPay Monitor</string>
  <key>CFBundleIdentifier</key><string>com.kunpay.monitor</string>
  <key>CFBundleExecutable</key><string>KunPayMonitor</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
  codesign --force --deep -s - dist/monitor/KunPayMonitor.app 2>/dev/null && echo "==> .app 已 ad-hoc 签名"
fi
build linux   amd64 "kunpay-monitor_linux64"
build linux   arm64 "kunpay-monitor_linux_arm64"

cat > dist/monitor/使用说明.txt <<'EOF'
KunPay 监控助手(Windows / macOS / Linux)

用途: 作为平台的"监控设备"保持在线(30秒心跳), 并提供手动/模拟到账上报,
用于全链路联调测试。微信/支付宝/QQ 收款通知的自动监听请使用安卓端 KunPay Monitor APP
(桌面系统无法读取手机收款通知)。

使用方法:
  1. 在平台后台「监控设备」中添加设备并复制 Token
  2. Windows: 双击 KunPayMonitor_win64.exe
     macOS: 双击 KunPayMonitor.app(或终端执行 ./KunPayMonitor.app/Contents/MacOS/KunPayMonitor)
     首次打开如被 Gatekeeper 拦截: 右键 -> 打开 -> 再点"打开";
     或终端执行: xattr -d com.apple.quarantine KunPayMonitor.app
     端口 8210 被占用时会自动换用 8211-8219,控制台地址以程序日志为准
     会自动打开浏览器控制台 (默认 http://127.0.0.1:8210)
  3. 控制台中填入平台地址与设备Token, 点「保存并连接」
  4. 保持程序运行, 平台设备列表即显示在线; 可用「模拟到账」测试订单匹配全链路

命令行参数(可选):
  -port 8210              控制台端口
  -server http://xx       平台地址(写入配置)
  -token xx               设备Token(写入配置)
  -config 路径            配置文件路径 (默认 ./kunpay-monitor.json)
  -no-open                不自动打开浏览器

macOS 首次运行如被 Gatekeeper 拦截: 系统设置 -> 隐私与安全性 -> 仍要打开。
EOF
echo "==> 完成: dist/monitor/"
ls -lh dist/monitor/ | grep -v total
