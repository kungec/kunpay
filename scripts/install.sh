#!/usr/bin/env bash
# ============================================================================
#  坤支付 KunPay — Linux 一键安装脚本
#
#  安装:   curl -fsSL https://raw.githubusercontent.com/kungec/kunpay/main/scripts/install.sh | bash
#  卸载:   curl -fsSL https://raw.githubusercontent.com/kungec/kunpay/main/scripts/install.sh | bash -s uninstall
#  指定版本: KUNPAY_VERSION=v1.3.1 curl -fsSL ... | bash
#
#  自适应: Ubuntu/Debian/CentOS/RHEL/Rocky/Alma/Fedora/openSUSE/Arch/Alpine 等
#          x86_64 / aarch64 架构, systemd / OpenRC / SysV 服务管理
# ============================================================================
set -uo pipefail

REPO="kungec/kunpay"
INSTALL_DIR="/opt/kunpay"
DATA_DIR="/opt/kunpay/data"
SERVICE_NAME="kunpay"
SERVICE_USER="kunpay"
BIN="$INSTALL_DIR/kunpay"
VERSION="${KUNPAY_VERSION:-latest}"

C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_BLUE=$'\033[36m'; C_END=$'\033[0m'
info()  { echo "${C_BLUE}[信息]${C_END} $*"; }
ok()    { echo "${C_GREEN}[完成]${C_END} $*"; }
warn()  { echo "${C_YELLOW}[警告]${C_END} $*"; }
die()   { echo "${C_RED}[错误]${C_END} $*" >&2; exit 1; }

# ---------- 卸载 ----------
if [ "${1:-}" = "uninstall" ] || [ "${1:-}" = "-u" ] || [ "${1:-}" = "--uninstall" ]; then
    [ "$(id -u)" -eq 0 ] || die "卸载需要 root 权限,请用 sudo 重试"
    info "停止服务..."
    if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
        systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f /etc/systemd/system/"$SERVICE_NAME".service
        systemctl daemon-reload 2>/dev/null || true
    elif command -v rc-service >/dev/null 2>&1; then
        rc-service "$SERVICE_NAME" stop 2>/dev/null || true
        rc-update delete "$SERVICE_NAME" 2>/dev/null || true
        rm -f /etc/init.d/"$SERVICE_NAME"
    else
        service "$SERVICE_NAME" stop 2>/dev/null || true
        update-rc.d -f "$SERVICE_NAME" remove 2>/dev/null || chkconfig "$SERVICE_NAME" off 2>/dev/null || true
        rm -f /etc/init.d/"$SERVICE_NAME"
    fi
    info "删除程序文件(数据目录 $DATA_DIR 保留)..."
    rm -rf "$INSTALL_DIR/kunpay" "$INSTALL_DIR/部署说明.txt"
    userdel "$SERVICE_USER" 2>/dev/null || true
    ok "已卸载。数据库与收款码等数据保留在 $DATA_DIR,如需彻底删除请手动清理。"
    exit 0
fi

# ---------- 环境检查 ----------
[ "$(uname -s)" = "Linux" ] || [ "${KUNPAY_SKIP_OS_CHECK:-0}" = "1" ] || die "本脚本仅支持 Linux;macOS/Windows 请从 Releases 页面下载对应产物"
[ "$(id -u)" -eq 0 ] || die "安装需要 root 权限,请用 sudo bash 重试"

ARCH=$(uname -m)
case "$ARCH" in
    x86_64)          ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    armv7l|armv6l)   die "暂未提供 armv7 产物,如有需要请在 GitHub 提 Issue" ;;
    *)               die "不支持的架构: $ARCH" ;;
esac
info "系统架构: linux/$ARCH"

# 下载工具: curl 或 wget 二选一
fetch() { # fetch <url> <outfile>
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 15 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --timeout=15 -O "$2" "$1"
    else
        die "需要 curl 或 wget 之一,请先安装 (apt/yum install curl)"
    fi
}

# ---------- 解析下载地址 ----------
ASSET="kunpay_linux_${ARCH}.tar.gz"
if [ "$VERSION" = "latest" ]; then
    DL_URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
    DL_URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
fi
info "下载地址: $DL_URL"

TMPDIR_DL=$(mktemp -d)
trap 'rm -rf "$TMPDIR_DL"' EXIT
info "正在下载 $ASSET ..."
fetch "$DL_URL" "$TMPDIR_DL/pkg.tar.gz" || die "下载失败: 请检查网络(国内服务器可先 export https_proxy=http://代理:端口)与版本号是否存在于 Releases"

if command -v sha256sum >/dev/null 2>&1; then
    GOT=$(sha256sum "$TMPDIR_DL/pkg.tar.gz" | awk '{print $1}')
    echo "  sha256: $GOT"
    # 供应链: KUNPAY_SHA256=<期望值> 时强制校验(不匹配立即终止)
    if [ -n "${KUNPAY_SHA256:-}" ] && [ "$GOT" != "${KUNPAY_SHA256,,}" ]; then
        die "SHA-256 校验失败! 期望 ${KUNPAY_SHA256,,} 实际 $GOT —— 疑似下载被篡改,已终止安装"
    fi
fi

# ---------- 解压安装 ----------
mkdir -p "$INSTALL_DIR" "$DATA_DIR"
tar -xzf "$TMPDIR_DL/pkg.tar.gz" -C "$TMPDIR_DL"
PKG_DIR=$(find "$TMPDIR_DL" -maxdepth 1 -type d -name "kunpay_linux_${ARCH}" | head -1)
[ -n "$PKG_DIR" ] && [ -f "$PKG_DIR/kunpay" ] || die "压缩包内容异常(未找到 kunpay 主程序)"

# 停止旧服务(升级场景)
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
elif command -v rc-service >/dev/null 2>&1; then
    rc-service "$SERVICE_NAME" stop 2>/dev/null || true
else
    service "$SERVICE_NAME" stop 2>/dev/null || true
fi

install -m 0755 "$PKG_DIR/kunpay" "$BIN"
[ -f "$PKG_DIR/部署说明.txt" ] && install -m 0644 "$PKG_DIR/部署说明.txt" "$INSTALL_DIR/" || true
ok "程序已安装到 $BIN"

# ---------- 专用系统用户 ----------
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$INSTALL_DIR" --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null \
        || adduser -S -D -H -s /sbin/nologin "$SERVICE_USER" 2>/dev/null \
        || warn "创建系统用户失败,服务将以 root 运行"
fi
chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR" 2>/dev/null || true
chown root:root "$BIN"

# ---------- 服务管理器自适应 ----------
write_unit() { # $1 = 输出路径
    cat > "$1" <<UNIT
[Unit]
Description=KunPay Payment System
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BIN} -port 8080 -db ${DATA_DIR}/kunpay.db -data ${DATA_DIR}
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT
}

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    write_unit /etc/systemd/system/"$SERVICE_NAME".service
    systemctl daemon-reload
    systemctl enable --now "$SERVICE_NAME"
    SVC_MGR="systemd"
elif command -v rc-service >/dev/null 2>&1; then
    cat > /etc/init.d/"$SERVICE_NAME" <<RC
#!/sbin/openrc-run
name="KunPay"
command="${BIN}"
command_args="-port 8080 -db ${DATA_DIR}/kunpay.db -data ${DATA_DIR}"
directory="${INSTALL_DIR}"
command_background="yes"
pidfile="/run/${SERVICE_NAME}.pid"
output_log="/var/log/${SERVICE_NAME}.log"
error_log="/var/log/${SERVICE_NAME}.err"
depend() { need net; }
RC
    chmod +x /etc/init.d/"$SERVICE_NAME"
    rc-update add "$SERVICE_NAME" default 2>/dev/null || true
    rc-service "$SERVICE_NAME" start || true
    SVC_MGR="OpenRC"
else
    cat > /etc/init.d/"$SERVICE_NAME" <<SYSV
#!/bin/bash
# chkconfig: 2345 90 10
# description: KunPay Payment System
case "\$1" in
  start) nohup ${BIN} -port 8080 -db ${DATA_DIR}/kunpay.db -data ${DATA_DIR} >>/var/log/${SERVICE_NAME}.log 2>&1 & echo started ;;
  stop)  pkill -f "${BIN}" || true; echo stopped ;;
  restart) \$0 stop; sleep 1; \$0 start ;;
  *) echo "usage: \$0 {start|stop|restart}" ;;
esac
SYSV
    chmod +x /etc/init.d/"$SERVICE_NAME"
    update-rc.d "$SERVICE_NAME" defaults 2>/dev/null || chkconfig "$SERVICE_NAME" on 2>/dev/null || true
    service "$SERVICE_NAME" start || true
    SVC_MGR="SysV"
fi
ok "服务已注册并启动(${SVC_MGR})"

# ---------- 等待就绪 ----------
sleep 2
HEALTH=""
if command -v curl >/dev/null 2>&1; then
    HEALTH=$(curl -fsS --connect-timeout 3 "http://127.0.0.1:8080/healthz" 2>/dev/null || true)
elif command -v wget >/dev/null 2>&1; then
    HEALTH=$(wget -qO- --timeout=3 "http://127.0.0.1:8080/healthz" 2>/dev/null || true)
fi

LOCAL_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1); exit}' || hostname -I 2>/dev/null | awk '{print $1}' || echo "服务器IP")

echo
echo "============================================================"
echo "  坤支付 KunPay 安装完成 🎉"
[ -n "$HEALTH" ] && echo "  服务状态: 运行中 ✓"
echo "  访问地址: http://${LOCAL_IP}:8080"
echo "  管理后台: http://${LOCAL_IP}:8080/admin"
echo "  默认账号: admin / admin123 (首次登录将强制要求修改密码)"
echo "  数据目录: ${DATA_DIR}"
echo
echo "  常用命令(systemd):"
echo "    systemctl status ${SERVICE_NAME}    # 查看状态"
echo "    journalctl -u ${SERVICE_NAME} -f    # 查看日志"
echo "    systemctl restart ${SERVICE_NAME}   # 重启服务"
echo
echo "  安全提示: 请尽快登录修改默认密码;生产环境请置于 HTTPS 反代之后。"
echo "  升级/卸载: 重新运行本脚本 / 加 uninstall 参数(数据目录保留)。"
echo "============================================================"
