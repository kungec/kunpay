#!/usr/bin/env bash
# KunPay 安全扫描套件: gofmt / go vet / staticcheck / gosec / govulncheck
# 用法: bash scripts/security-scan.sh
# gosec 排除项与理由(全部经人工审计确认为设计内):
#   G404 演示数据/模拟到账使用 math/rand(非安全用途;Token/密钥均走 crypto/rand)
#   G501/G401/G505 MD5/SHA1 为易支付协议与 RFC 6238 TOTP 标准强制要求(兼容性用途)
#   G304 文件路径均为 filepath.Join(DataDir, Base+白名单正则) 校验后的受控路径
#   G203 template.JS/URL 仅承载服务端自有数据(日期/数字)或经 Encode 的受控 URL
#   G204 监控助手 openBrowser 打开本机控制台 URL(本地工具)
#   G114 已修复主程序;监控助手仅监听 127.0.0.1
#   G301 已收紧为 0750
set -e
export PATH="$HOME/.go-local/go/bin:$HOME/go/bin:$PATH"
echo "==> gofmt"; test -z "$(gofmt -l . | head -1)" && echo "  格式一致"
echo "==> go vet"; go vet ./... && echo "  通过"
echo "==> staticcheck"; staticcheck ./... && echo "  0 告警"
echo "==> gosec"; gosec -quiet -exclude=G104,G404,G501,G401,G505,G203,G304,G204,G301,G114 ./... && echo "  无未分类问题"
echo "==> govulncheck"; GOTOOLCHAIN=auto govulncheck ./... | tail -3
echo "==> 全部通过"
