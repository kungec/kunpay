#!/usr/bin/env bash
# 构建 Docker 镜像(自动先编译 linux/amd64 二进制)
set -e
cd "$(dirname "$0")"
export CGO_ENABLED=0
echo "==> 编译 linux/amd64"
mkdir -p dist/docker
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/docker/kunpay ./cmd/server
echo "==> 构建 Docker 镜像 kunpay:latest"
docker build -f Dockerfile -t kunpay:latest .
echo "==> 完成。运行: docker run -d -p 8080:8080 -v kunpay_data:/app/data --name kunpay kunpay"
