#!/usr/bin/env bash
# KunPay 一键交叉编译脚本 (macOS / Linux 上运行)
# 产物输出到 dist/ 目录,全部为免 CGO 的单文件二进制,可直接部署。
set -e
cd "$(dirname "$0")"

export CGO_ENABLED=0
VERSION=$(date +%Y%m%d)
LDFLAGS="-s -w"

echo "==> 清理 dist/"
rm -rf dist
mkdir -p dist

build() {
  local os=$1 arch=$2 out=$3
  echo "==> 编译 $os/$arch -> $out"
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/$out" ./cmd/server
}

build linux   amd64 "kunpay_linux_amd64/kunpay"
build linux   arm64 "kunpay_linux_arm64/kunpay"
build windows amd64 "kunpay_windows_amd64/kunpay.exe"
build windows arm64 "kunpay_windows_arm64/kunpay.exe"
build darwin  arm64 "kunpay_darwin_arm64/kunpay"
build darwin  amd64 "kunpay_darwin_amd64/kunpay"

# 附带示例配置与说明
for d in dist/kunpay_*/; do
  cat > "$d/部署说明.txt" <<EOF
KunPay 码支付系统 - 启动说明

1. 将 kunpay(或 kunpay.exe)上传到服务器任意目录
2. 启动:
   Linux/macOS:  chmod +x kunpay && ./kunpay -port 8080
   Windows:      双击 kunpay.exe 或 cmd 运行 kunpay.exe -port 8080
3. 访问 http://服务器IP:端口/
   管理后台: /admin   默认账号 admin / admin123 (请立即修改)
4. 数据库文件 kunpay.db 与 data/ 目录自动生成在运行目录,请定期备份

可选参数:
  -port 8080    HTTP 监听端口 (环境变量 KUNPAY_PORT)
  -db   路径     SQLite 数据库文件路径 (环境变量 KUNPAY_DB)
  -data 路径     收款码等上传文件目录 (环境变量 KUNPAY_DATA)

Systemd 部署示例 (Linux):
  [Unit]
  Description=KunPay
  After=network.target
  [Service]
  WorkingDirectory=/opt/kunpay
  ExecStart=/opt/kunpay/kunpay -port 8080
  Restart=always
  [Install]
  WantedBy=multi-user.target

建议使用 Nginx/Caddy 反向代理并配置 HTTPS 后对外提供服务。
EOF
done

echo "==> 完成,产物清单:"
ls -lh dist/*/ | grep -v total
