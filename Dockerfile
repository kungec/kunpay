# KunPay 码支付系统 Docker 镜像(多阶段构建,自包含,支持 amd64/arm64)
# 本地: docker build -t kunpay .
# 运行: docker run -d -p 8080:8080 -v kunpay_data:/app/data --name kunpay kunpay

FROM golang:1-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/kunpay ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

RUN addgroup -S kunpay && adduser -S kunpay -G kunpay
WORKDIR /app
COPY --from=builder /out/kunpay /app/kunpay
RUN chmod +x /app/kunpay && mkdir -p /app/data && chown -R kunpay:kunpay /app

USER kunpay
EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz > /dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/kunpay"]
