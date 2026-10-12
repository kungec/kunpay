# KunPay 码支付系统 Docker 镜像
# 构建: 先运行 bash build.sh 得到 linux 二进制,然后:
#   docker build -t kunpay .
# 运行:
#   docker run -d -p 8080:8080 -v kunpay_data:/app/data --name kunpay kunpay
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app
COPY dist/kunpay_linux_amd64/kunpay /app/kunpay
RUN chmod +x /app/kunpay && mkdir -p /app/data

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/ > /dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/kunpay", "-port", "8080", "-db", "/app/data/kunpay.db", "-data", "/app/data"]
