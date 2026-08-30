# nvr2rtc — TP-Link NVR 实时预览 → HTTP MPEG-TS

把 TP-Link NVR（已验证 TL-NVR6108C-L，其他型号协议同源）的实时预览流以标准
**HTTP MPEG-TS** 暴露出去，供 go2rtc / ffmpeg / VLC / ffprobe / Home Assistant 直接拉取。

只需提供 NVR 管理员账号密码，无其他配置：

```bash
nvr2rtc --pass <NVR密码>            # 默认 user=admin, nvr=192.168.0.49, http=0.0.0.0:8081
nvr2rtc --user admin --pass 123456  --ch 1,3,4,6
```

- 输出端点：`http://<host>:8081/ch/<N>`（N = 通道号，**1 起**，与 TP-Link 客户端的
  第 1/N 路一一对应；协议内部 API 通道为 N-1，见 PROTOCOL.md）
- 默认**透传原始流**（含 TP-Link 私有流 0x92 与 null 填充）；加 `--clean` 开关做 TS 清洗
  （只保留视频 PID，剔除私有流）——清洗不影响启动速度，只是让下游工具看到的流更干净
- 断流自动重连；**同一通道多个客户端共享一路 NVR 会话（扇出）**，多通道各自独立连接 NVR

> 共享会话：同一通道被多个客户端（ffplay / go2rtc / VLC）同时拉取时，NVR 侧只建立
> **一路** preview 会话，服务端把流扇出给所有客户端（各带独立下行缓冲，慢客户端
> 溢出只丢自己的帧，互不影响）。最后一个客户端断开后，会话保留 1 分钟供新客户端
> 直接复用，之后自动关闭，不占 NVR 会话数（NVR `stream_max_sessions=8`）。

## 参数

| Flag | 默认 | 说明 |
|---|---|---|
| `--pass` | （必填） | NVR 管理员密码。可用环境变量 `NVR2RTC_PASS` |
| `--user` | `admin` | NVR 用户名，可用 `NVR2RTC_USER` |
| `--nvr` | `192.168.0.49:8000` | NVR 地址（host 或 host:port），可用 `NVR2RTC_NVR` |
| `--http` | `0.0.0.0:8081` | HTTP 监听地址，可用 `NVR2RTC_HTTP` |
| `--ch` | 不限制 | 通道白名单（逗号分隔，1 起，如 `1,3,4,6`）。**默认不限制**——任意正整数通道都放行，
  有没有数据由 NVR 决定（不同型号路数不同，如 8/16/32 路）；指定后其余通道返回 403 |
| `--clean` | 关 | 清洗 TS：剔除 TP-Link 私有流(0x92)，只保留视频流。默认透传原始流 |

## 接入 go2rtc

```yaml
streams:
  cam1:
    ffmpeg: http://192.168.0.60:8081/ch/1#video=copy
  cam3:
    ffmpeg: http://192.168.0.60:8081/ch/3#video=copy
```

> **重要**：ffmpeg 系客户端默认 `-probesize 5MB`，对 1× 实时流（~250KB/s）要读满
> 5MB 探测才开始解码 ≈ 20 秒。**给 ffmpeg 加参数即秒开**：

```yaml
streams:
  cam1:
    ffmpeg2:
      - -probesize
      - "32768"
      - -analyzeduration
      - "100000"
      - -i
      - http://192.168.0.60:8081/ch/1
```

命令行直接拉流：

```bash
ffmpeg -probesize 32768 -analyzeduration 100000 -i http://<host>:8081/ch/1 -c copy out.ts
ffprobe http://<host>:8081/ch/1
```

## 构建

```bash
cd nvr2rtc
GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOFLAGS=-mod=mod go build -o nvr2rtc .
```

纯标准库，静态编译后拷到 NAS/Linux 即可。

## 部署（systemd 示例）

```ini
# /etc/systemd/system/nvr2rtc.service
[Unit]
Description=nvr2rtc TP-Link NVR HTTP-TS stream
After=network-online.target

[Service]
ExecStart=/nas/nvr2rtc/nvr2rtc --pass 你的密码 --ch 1,3,4,6
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## 目录结构

```
main.go        CLI + HTTP 服务器
tplink/
  tplink.go    NVR 协议: securityEncode、Digest、preview 会话、信用流控
  tsclean.go   TS 清洗: PMT 重写剔除私有流(0x92)
```

协议细节见仓库根 `PROTOCOL.md`。