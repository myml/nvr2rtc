# nvr2rtc — TP-Link NVR 实时预览 → HTTP MPEG-TS

把 TP-Link NVR（已验证 TL-NVR6108C-L，其他型号协议同源）的实时预览流以标准
**HTTP MPEG-TS** 暴露出去，供 go2rtc / ffmpeg / VLC / ffprobe / Home Assistant 直接拉取。

只需提供 NVR 管理员账号密码，无其他配置：

```bash
nvr2rtc --pass <NVR密码>            # 默认 user=admin, nvr=192.168.0.49, http=0.0.0.0:8081
nvr2rtc --user admin --pass 123456  --ch 1,3,4,6
```

- 输出端点：
  - `http://<host>:8081/ch/<N>` — **主码流**（高清，HEVC 2560×1440）
  - `http://<host>:8081/sub/<N>` — **子码流**（低清，H.264 640×480）

  N = 通道号，**1 起**，与 TP-Link 客户端的第 1/N 路一一对应（协议内部 API 通道为 N-1，见 PROTOCOL.md）。
  两条路径都是固定档位、互不影响；同一个通道的主/子码流各占一路 NVR 会话。
- 默认**透传原始流**（含 TP-Link 私有流 0x92 与 null 填充）；加 `--clean` 开关做 TS 清洗
  （只保留视频 PID，剔除私有流）——清洗不影响启动速度，只是让下游工具看到的流更干净
- 断流自动重连；**同一通道同一档位多个客户端共享一路 NVR 会话（扇出）**，多通道/多档位各自独立连接 NVR

> 共享会话：同一通道同一档位被多个客户端（ffplay / go2rtc / VLC）同时拉取时，NVR 侧只建立
> **一路** preview 会话，服务端把流扇出给所有客户端（各带独立下行缓冲，慢客户端
> 溢出只丢自己的帧，互不影响）。最后一个客户端断开后，会话保留 1 分钟供新客户端
> 直接复用，之后自动关闭，不占 NVR 会话数（NVR `stream_max_sessions=8`）。
>
> **主/子码流是两个独立会话**：订阅 `/ch/1` 与 `/sub/1` 会占 2 路 NVR 会话。低码率场景
> 建议优先用 `/sub/<N>`（640×480 H.264，约 30KB/s，是主码流 ~275KB/s 的 1/9）。

## 参数

| Flag | 默认 | 说明 |
|---|---|---|
| `--pass` | （必填） | NVR 管理员密码。可用环境变量 `NVR2RTC_PASS` |
| `--user` | `admin` | NVR 用户名，可用 `NVR2RTC_USER` |
| `--nvr` | `192.168.0.49:8000` | NVR 地址（host 或 host:port），可用 `NVR2RTC_NVR` |
| `--http` | `0.0.0.0:8081` | HTTP 监听地址，可用 `NVR2RTC_HTTP` |
| `--ch` | 不限制 | 通道白名单（逗号分隔，1 起，如 `1,3,4,6`）。**默认不限制**——任意正整数通道都放行，
  有没有数据由 NVR 决定（不同型号路数不同，如 8/16/32 路）；指定后其余通道返回 403。
  白名单同时作用于 `/ch/` 与 `/sub/` |
| `--clean` | 关 | 清洗 TS：剔除 TP-Link 私有流(0x92)，只保留视频流。默认透传原始流 |

## 接入 go2rtc

```yaml
streams:
  cam1:          # 主码流
    ffmpeg: http://192.168.0.60:8081/ch/1#video=copy
  cam1_sub:      # 子码流（省带宽，适合多路预览/手机端）
    ffmpeg: http://192.168.0.60:8081/sub/1#video=copy
  cam3:
    ffmpeg: http://192.168.0.60:8081/ch/3#video=copy
```

> **重要**：ffmpeg 系客户端默认 `-probesize 5MB`，对 1× 实时流（主码流 ~250KB/s）要读满
> 5MB 探测才开始解码 ≈ 20 秒。**给 ffmpeg 加参数即秒开**（子码流码率低，默认探测更慢，
> 更需要加）：

```yaml
streams:
  cam1_sub:
    ffmpeg2:
      - -probesize
      - "32768"
      - -analyzeduration
      - "100000"
      - -i
      - http://192.168.0.60:8081/sub/1
```

命令行直接拉流：

```bash
ffmpeg -probesize 32768 -analyzeduration 100000 -i http://<host>:8081/ch/1  -c copy out.ts   # 主码流
ffmpeg -probesize 32768 -analyzeduration 100000 -i http://<host>:8081/sub/1 -c copy out.ts   # 子码流
ffprobe http://<host>:8081/sub/1
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
main.go        CLI + HTTP 服务器 (/ch/ 主码流, /sub/ 子码流)
tplink/
  tplink.go    NVR 协议: securityEncode、Digest、preview 会话、档位、信用流控
  tsclean.go   TS 清洗: PMT 重写剔除私有流(0x92)
  hub_test.go  共享会话扇出 / 档位 / 请求体 单元测试
probe/         一次性逆向工具: 爆破 preview 请求参数(见下)
```

## resolutions 档位逆向（probe/）

NVR 的 `preview` 请求用 `resolutions` 字段选码流，但**不能按字面分辨率理解**——实测
（2026-09-20，固件 1.0.25）该字段在固件里只做**子串匹配**：

| `resolutions` 取值 | 实际结果 |
|---|---|
| `"HD"`（含 "HD"） | 主码流 HEVC 2560×1440 15fps（~275KB/s） |
| `"VGA"` / `"QVGA"` / `"SVGA"`（含 "VGA"） | 子码流 H.264 640×480 15fps（~30KB/s） |
| `"VGAHD"`（两者都含） | 主码流（**HD 优先**） |
| `"SD"` / `"LD"` / `"1080P"` / `"MAIN"` / `"D1"` | **HTTP 200 但零字节**（静默空流） |

推论与证据：

- 名字本身无意义：`AAVGABB`、`NOTVGA` 同样命中子码流，`xxHDxx`、`hdmi` 同样命中主码流；
- `VGA` 与 `QVGA` **是同一路流**：码率相同、`ffprobe` 编码参数相同、**SPS 逐字节相同**
  （SHA1 一致）——不存在比 640×480 更低的子码流；
- 因此对外只暴露两档（`/ch/` 主码流、`/sub/` 子码流），不把误导性的 `VGA`/`QVGA` 名字暴露给用户。

`probe/` 保留在仓库里，方便换型号/固件后复跑参数矩阵：

```bash
go run ./probe -pass <密码> -ch 0 -secs 4                 # 跑内置用例矩阵
go run ./probe -pass <密码> -ch 0 -secs 8 -res VGA -out a.ts   # 单个取值并 dump 原始 TS
ffprobe a.ts                                              # 看真实分辨率/编码
```

协议细节见仓库根 `PROTOCOL.md`。