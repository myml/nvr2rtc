# TP-Link NVR (TL-NVR6108C-L) 私有协议参考

> 逆向来源：固件 rootfs 字符串/反汇编 + 手机 App（TP-Link 官方）8000 端口抓包
> （`captures/tcp_*.pcap`，PCAPDroid 原始 IP 链路，linktype=101）+ 逐字节实测比对。
> 验证时间：2026-08-23 ~ 2026-08-26，固件 1.0.25（Build 260609 Rel.51359n）。
> Go 实现见 `nvrdl/`；本文档为目标读者是"想不靠 App 直接操作这台 NVR"的开发者。

设备有两套 HTTP 服务器，职责完全不同：

| 服务器 | 端口 | 用途 | 实现 |
|---|---|---|---|
| Web 管理 API（`/ds`） | 80 / 443 | 登录、文件列表、日期、事件、用户 ID | `usr/bin/dsd` |
| 流服务器 | 8000 | 实时预览、回放、录像下载（MPEG-TS） | `bin/nvrcore`（响应头 `Server: NVRHttpStream`） |

---

## 1. 密码：securityEncode

TP-Link 经典混淆算法，三处都用它：

1. `/ds` 登录的 `login.password`
2. 8000 端口 HTTP Digest 的密码
3. 某些固件里 `login2` 的 password 也可能走它

```
SALT = "RDpbLfCPsJZ7fiv"
KEY  = "e8b2a3d0c4f5a9b7c1d3e6f0a2b4c8d5e7f9a1b3c6d8e0"  # 实际长串以固件为准（见下）
```

算法（与 TP-Link 老款路由器一致）：
```
def securityEncode(password):
    salt = "RDpbLfCPsJZ7fiv"
    key  = "....长 KEY 串...."
    a, b = [], []
    for ch in key: a.append(ord(ch))
    for ch in salt: b.append(ord(ch))
    state = 0
    # 逐字符对 password 和 salt 做移位运算
```
确切实现可直接读 `nvrdl/main.go` 的 `securityEncode`（与本型号逐字节比对待验）。
注意：**`securityEncode` 的输出同时是 Digest 密码**，即 Digest 里 `username=admin`，
`password` 不是明文而是 securityEncode 后的结果。

---

## 2. Web 管理 API（/ds）

### 2.1 登录

```
POST / HTTP/1.1
Host: <nvr>:80
Content-Type: application/json

{"method":"do","login":{"username":"admin","password":"<securityEncode(明文)>"}}
```

成功返回：
```json
{"stok":"<token>","user_group":"root","timestamp":...}
```
后续 `/ds` 调用都要带 stok：`POST /stok=<token>/ds`。

### 2.2 获取 user_id（下载协议需要）

```
POST /stok=<token>/ds
{"system":{"get_user_id":null},"method":"do"}
```
返回形如 `{"system":{"get_user_id":7},"error_code":0}` —— 整数 user_id，
App 在 download 请求里用它做 `client_id`。

### 2.3 文件列表 media.get_media_list

```
POST /stok=<token>/ds
{"media":{"get_media_list":{
    "channel":["0"],                 // 必须字符串数组，数字会 -71112
    "start_time":"1787414400",       // 必须是字符串（unix 秒），数字会 -71112
    "end_time":"1787417563",         // 同上
    "event_type":[1,2],              // 服务端会忽略这个过滤！（见下）
    "media_type":[0],
    "start_index":0,                 // 分页起点
    "max_num":100,                   // 单页上限 100，超了自动分页
    "user_id":7
}},"method":"do"}
```

实测要点：
- **时间粒度只有"天"**：窄窗口（如 1 小时）也返回全天段，App 同款行为；
- **event_type 过滤无效**：服务端忽略，返回的段由客户端自己按
  `event_type == 1`（连续录像）/ `!= 1`（活动录像）过滤。验证：1183 段 = 25 连续 + 1158 活动；
- 每页最多 100 条，超了要 `start_index += 100` 翻页（工具 cap 10000）；
- 返回条目：`{start_time, end_time, size, event_type, ...}`，size 为字节。

### 2.4 其他已确认接口

| 接口 | 说明 | 状态 |
|---|---|---|
| `search_year` | 有录像的年份/日期 | ✅ 真数据（`dates` 命令用） |
| `get_event_list` | 事件段 | ✅ 真数据 |
| `system.get_user_id` | user_id | ✅ |
| `media.search_video` | 连续录像文件表 | ❌ 1.0.25 上所有参数 -40209（未绑定，走媒体会话协议） |
| `backup_storage` / `backup_service` | NAS/FTP 备份 | ❌ 1.0.22 与 1.0.25 均不存在（-40209 校准确认） |

---

## 3. 8000 流服务器

### 3.1 会话握手（两步 POST）

统一入口 `POST /stream`，HTTP/1.1，`Content-Length: -1`（Go 标准库会拒绝，需原生 TCP）。

```
POST /stream HTTP/1.1
Content-Length: -1
X-Client-UUID: <32位hex>
X-Client-Model: Android
Connection: keep-alive
Host: <nvr>:8000
Content-Type: multipart/mixed;boundary=--client-stream-boundary--
```

- **第一步**：只发上面的头（无 body），收到 `401 + WWW-Authenticate: Digest ...`
  （`realm="TP-Link IP-Camera"`, 含 `nonce`/`opaque`）；
- **第二步**：带上 `Authorization: Digest` 重发（含 body）。
  Digest 关键：**所有参数值必须带双引号**（`username="admin", realm="...", nonce="...", uri="/stream",
  qop=auth, nc=00000001, cnonce="...", response="...", opaque="..."`），否则 401/被拒；
- 第二步成功后返回 `HTTP/1.0 200` + multipart 流。

### 3.2 请求消息（multipart JSON 信封）

body 由若干 part 组成，全部用边界 `----client-stream-boundary--` 分隔：

```
----<--client-stream-boundary---->--
Content-Type: application/json
X-Data-Window-Size: 50
Content-Length: <N>

{"type":"request","seq":0,"params":{...}}
[后面紧跟 10 个预发信用通知 part: X-Data-Received 25,50,...,250（+stream_sequence），
 每个 part 也要带 ----client-stream-boundary-- 边界]
```

**结尾没有闭合分隔符**（`----client-stream-boundary----` 不存在）——实测 App 也是如此，别加。

### 3.3 三种请求消息（App 抓包原文）

**实时预览 preview**（serve 用）：
```json
{"type":"request","seq":0,"params":{"method":"get","preview":{
    "channels":[0],
    "privary_auth":[0],
    "resolutions":["HD"]
}}}
```
→ 返回实时 TS，`video/mp2t` part。

**回放 playback2**（App 搜录像时发，本次抓包新确认）：
```json
{"type":"request","seq":0,"params":{"method":"get","playback2":{
    "channels":[0],
    "privary_auth":[0],
    "client_id":"e6c746b2-20f7-4800-b694-36f046a8bff91865a67ab7e",  // ← 字符串 UUID！
    "scale":"1/1",
    "start_time":"1787500800",
    "end_time":"1787587199",
    "timestamp":"18446744073709551615",
    "event_type_exclude":[88]
}}}
```
注意 playback2 里 `client_id` 是 **UUID 字符串**（与 download 的整数不同）。

**下载 download**（dlfile 用）：
```json
{"type":"request","seq":0,"params":{"method":"get","download":{
    "channels":[0],
    "privary_auth":[0],
    "client_id":7,                 // ← 整数（get_user_id 得到的）
    "media_type":0,
    "start_time":"1787502811",
    "end_time":"1787502823",
    "event_type":["21"]
}}}
```

**结束**（三种都发）：
```json
{"type":"request","seq":1,"params":{"method":"do","stop":"null"}}
```

### 3.4 信用流控（必须持续回发确认，否则截断）

- 请求带 `X-Data-Window-Size: 50`：服务器按"窗口"发数据，**每窗口约 50 个 part ≈ 600KB**；
- 客户端必须周期性回发 **`stream_sequence` 通知**（`X-Data-Received` 累加 +25），
  否则服务器发完 ~600KB 就停 → 下载大概 20 秒后静默截断（曾误判成功）；
- 实测节奏：每 **~2 秒** 回发一个，`X-Data-Received` 从 275 起 +25。App 在对话框 body 里
  预发了 10 个（25→250），下载中继续回发；
- 通知格式：
```
----<--client-stream-boundary---->--
X-Session-Id: <sessionID>
Content-Type: application/json
X-Data-Received: 300
Content-Length: <N>

{"type":"notification","params":{"event_type":"stream_sequence"}}
```

### 3.5 响应格式

```
HTTP/1.0 200
Content-Type: multipart/mixed;boundary=--device-stream-boundary--

----device-stream-boundary--
Content-Type: application/json
Content-Length: <N>

{"type":"...","params":{"session_id":"...","stream_status":"start"}}
----device-stream-boundary--
Content-Type: video/mp2t
Content-Length: <N>

<188字节×N 的裸 TS 包...>
  ... 分隔符为 \r\n----device-stream-boundary--
  结束：JSON part 里 stream_status = finished
```

`X-Session-Id` 从响应 JSON part 里提取（`session_id`），后续回发确认要用。

### 3.6 媒体流结构（本次抓包新确认，preview）

原始 TS 里（PID 统计实测）：

| PID | 值 | 内容 |
|---|---|---|
| 0x0000 (0) | PAT | 少量（流开头 4 次左右） |
| 0x0012 (18) | PMT | program=1, PCR pid=0x44 |
| 0x0044 (68) | 视频 | `stream_type 0x24` = HEVC，主码流 2560×1440（15fps 左右） |
| 0x0045 (69) | 私有流 | `stream_type 0x92`，内容是 0xd5 填充 —— **App 认识/忽略，ffmpeg 不认** |
| 0x1fff (8191) | null | 填充 |

PMT 原文：
```
program=1  pcr_pid=0x44
  stream_type=0x24 es_pid=0x44   (HEVC 视频)
  stream_type=0x92 es_pid=0x45   (TP-Link 私有流, 0xd5 填充)
```

**给 ffmpeg/go2rtc 前应先清洗**：重写 PMT 只保留 `0x24` ES、重算 section_length + MPEG CRC32，
并只转发 PAT/PMT/视频 PID（实现见 `nvrdl/serve.go` 的 `tsCleaner`）。
拿到 PMT 前的开头视频包会丢弃（学习期，几毫秒，无感）。

### 3.7 scale 参数（回放调速，实测表）

| scale | 行为 | 用途 |
|---|---|---|
| 1/1 | 实时无损，全帧 | ✅ 唯一无损档（默认） |
| 2/1 | 无提速，数据≈2×（帧加倍） | ❌ |
| 3/1 | 服务端不支持，回落 1/1 | = 1/1 |
| 4/1, 8/1, 16/1, 32/1 | 丢帧快进（仅关键帧 ~240KB） | 快速检索 |

下载速率被协议锁死在**录像实时码率**（1 小时段≈1 小时）；想提速只能多路并行
（NVR `stream_max_sessions=8`）。

---

## 4. 错误码

| 错误码 | 含义 | 处理 |
|---|---|---|
| -40209 | 未知模块/该固件未实现该接口 | 用假模块名校准过，可信 |
| -40106 | 无效指令 | 参数结构错 |
| -40101 | 模块已实现但查询为空 | 无数据 |
| -71112 | 参数类型错（如 get_media_list 传数字 channel/时间） | 改字符串 |
| -52402 | 媒体会话并发占满/busy/stale | 等几秒重试 |

---

## 5. 已知限制 / 事实

- **没有原生 FTP/NAS 备份**：`backup_storage`/`backup_service` 在 1.0.22 和 1.0.25 都不存在；
  固件里的 `ftp_upload/ftp_download` 字符串是 nvrtest 自测项；`ConfLocalStorageBackup.htm`
  是共享遗留页面。**NAS 自动备份只能靠外部工具拉录像**（本仓库 nvrdl 就是干这个的）；
- 通道编号：API `channel = 物理通道-1`（0=第1路…本机 0,2,3,5 = 物理 1/3/4/6 路）。
  **本仓库 HTTP 端点 `/ch/<N>` 用物理通道号（1 起），提前做了 `N-1` 换算**（见 main.go）；
- 单条流速率 = 实时码率（HEVC 2K 约 2~2.5Mbps）；8 路并发是协议给的上限；
- ffmpeg/VLC 打开本工具的 HTTP-TS 流很慢是 **ffmpeg 默认 probesize=5MB** 的探测行为，
  与 NVR 协议无关；加 `-probesize 32768 -analyzeduration 100000` 即秒开（见 nvrdl/README.md）。

---

## 6. 固件逆向参考（refs/）

- `refs/bin/nvr_fw_1.0.25.bin`：从 NVR 云端升级 URL 拉的原厂固件；
- `refs/bin/squashfs.bin`（偏移 1836032）、`refs/bin/rootfs/`：解包后的文件系统；
- 关键二进制：
  - `bin/nvrcore`：NVRHttpStream（8000 流服务器）；
  - `usr/bin/dsd`：/ds API daemon；
  - `gui/nvrgui5`：UI/媒体 daemon；
- `refs/bin/nvrplugin.exe` + 解包目录：Windows 插件（含 securityEncode 等参考实现）。

## 7. 抓包文件（captures/）

PCAPDroid 原始 IP 链路（**linktype 101，无 Ethernet 头**，dpkt 需 `dpkt.ip.IP` 直接解）；
手机在 VPN 网段 `10.215.173.1`，NVR 真实 IP `192.168.0.49`；分析脚本 `captures/analyze_pcap.py`。