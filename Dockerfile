# syntax=docker/dockerfile:1

# ---- 构建阶段: 编译静态 Go 二进制 ----
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
# 先复制 go.mod 以便利用层缓存(本项目无第三方依赖,无 go.sum)
COPY go.mod ./
COPY main.go ./
COPY tplink/ ./tplink/
RUN go build -trimpath -ldflags="-s -w" -o /out/nvr2rtc .

# ---- 运行阶段: 最小运行时镜像 ----
FROM alpine:3.21
RUN addgroup -S nvr2rtc && adduser -S -G nvr2rtc nvr2rtc
COPY --from=build /out/nvr2rtc /usr/local/bin/nvr2rtc
USER nvr2rtc
EXPOSE 8081
# 默认监听 0.0.0.0:8081;NVR 连接由 NVR2RTC_NVR / NVR2RTC_USER / NVR2RTC_PASS 配置
ENV NVR2RTC_HTTP=0.0.0.0:8081
ENTRYPOINT ["/usr/local/bin/nvr2rtc"]