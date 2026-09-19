// probe: 逆向探测工具 —— 对 NVR 8000 preview 请求做参数矩阵实验，找码流档位写法。
//
// 用法:
//
//	# 跑内置用例矩阵（各种 resolutions 取值 + 若干猜测字段）
//	go run ./probe -addr 192.168.0.49:8000 -user admin -pass <密码> -ch 0 -secs 4
//
//	# 只跑单个 resolutions 取值，并把原始 TS dump 到文件（再用 ffprobe 看真实分辨率）
//	go run ./probe -pass <密码> -ch 0 -secs 8 -res VGA -out /tmp/a.ts
//	ffprobe /tmp/a.ts
//
// 每个用例建立一路 preview 会话，读若干秒 TS，统计 PID 分布与字节速率，打印一行结果。
//
// 已用它确认（固件 1.0.25）：resolutions 只做子串匹配——含 "HD" → 主码流，
// 含 "VGA" → 子码流(640×480 H.264)，都不含 → HTTP 200 但零字节的静默空流。
// 详见仓库 README.md「resolutions 档位逆向」一节。
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const (
	streamBoundary = "client-stream-boundary"
	nvrRealm       = "TP-Link IP-Camera"
	tpSalt         = "RDpbLfCPsJZ7fiv"
)

const tpKey = "yLwVl0zKqws7LgKPRQ84Mdt708T1qQ3Ha7xv3H7NyU84p21BriUWBU43odz3iP4rBL3cD02KZci" +
	"XTysVXiV8ngg6vL48rPJyAUw0HurW20xqxv9aYb4M9wK1Ae0wlro510qXeU07kV57fQMc8L6aLgMLwygtc0F10a0Dg70TOoouy" +
	"FhdysuRMO51yY5ZlOZZLEal1h0t9YQW0Ko7oBwmCAHoic4HYbUyVeU3sfQ1xtXcPcf1aT303wAQhv66qzW"

func securityEncode(data string) string {
	d := max(len(data), len(tpSalt))
	var out strings.Builder
	for m := 0; m < d; m++ {
		k, l := 187, 187
		switch {
		case m >= len(data):
			l = int(tpSalt[m])
		case m >= len(tpSalt):
			k = int(data[m])
		default:
			k = int(data[m])
			l = int(tpSalt[m])
		}
		out.WriteByte(tpKey[(k^l)%len(tpKey)])
	}
	return out.String()
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func digestResp(nonce, opaque, user, pw string) string {
	ha1 := md5hex(user + ":" + nvrRealm + ":" + pw)
	ha2 := md5hex("POST:/stream")
	cn := "0a1b2c3d"
	nc := "00000001"
	r := md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cn + ":auth:" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="/stream", qop=auth, nc=%s, cnonce="%s", response="%s", opaque="%s"`,
		user, nvrRealm, nonce, nc, cn, r, opaque)
}

type testCase struct {
	name string
	body map[string]any
}

func main() {
	addr := flag.String("addr", "192.168.0.49:8000", "NVR 8000")
	user := flag.String("user", "admin", "user")
	pass := flag.String("pass", "", "pass")
	ch := flag.Int("ch", 0, "API channel (0 起)")
	secs := flag.Int("secs", 6, "每个用例读取秒数")
	res := flag.String("res", "", "只跑单个 resolutions 值并 dump 到 -out")
	out := flag.String("out", "", "dump 原始 TS 到文件")
	flag.Parse()
	if *pass == "" {
		fmt.Fprintln(os.Stderr, "need -pass")
		os.Exit(1)
	}
	enc := securityEncode(*pass)

	if *res != "" {
		dumpRun(*addr, *user, enc, *ch, *res, *out, time.Duration(*secs)*time.Second)
		return
	}

	base := func(extra map[string]any) map[string]any {
		p := map[string]any{
			"channels":     []int{*ch},
			"privary_auth": []int{0},
			"resolutions":  []string{"HD"},
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	cases := []testCase{
		{"baseline-HD", base(nil)},
		{"res-SD", base(map[string]any{"resolutions": []string{"SD"}})},

		{"res-SD+stream_type-1", base(map[string]any{"resolutions": []string{"SD"}, "stream_type": 1})},
		{"res-SD+streamtype-1", base(map[string]any{"resolutions": []string{"SD"}, "streamtype": 1})},
		{"res-SD+sub_stream-1", base(map[string]any{"resolutions": []string{"SD"}, "sub_stream": 1})},
		{"res-SD+main_sub-1", base(map[string]any{"resolutions": []string{"SD"}, "main_sub": 1})},
		{"res-SD+stream_index-1", base(map[string]any{"resolutions": []string{"SD"}, "stream_index": 1})},
		{"res-SD+stream-1", base(map[string]any{"resolutions": []string{"SD"}, "stream": 1})},
		{"res-SD+quality-1", base(map[string]any{"resolutions": []string{"SD"}, "quality": 1})},
		{"res-SD+resolution-1", base(map[string]any{"resolutions": []string{"SD"}, "resolution": 1})},
		{"res-SD+profile-1", base(map[string]any{"resolutions": []string{"SD"}, "profile": 1})},

		{"res-SD,SD", base(map[string]any{"resolutions": []string{"SD", "SD"}})},
		{"res-1", base(map[string]any{"resolutions": []int{1}})},
		{"res-2", base(map[string]any{"resolutions": []int{2}})},
		{"res-0", base(map[string]any{"resolutions": []int{0}})},
		{"res-LD", base(map[string]any{"resolutions": []string{"LD"}})},
		{"res-SUB", base(map[string]any{"resolutions": []string{"SUB"}})},
		{"res-VGA", base(map[string]any{"resolutions": []string{"VGA"}})},
		{"res-fluid", base(map[string]any{"resolutions": []string{"fluid"}})},
		{"res-smooth", base(map[string]any{"resolutions": []string{"smooth"}})},
		{"res-empty", base(map[string]any{"resolutions": []string{}})},
		{"no-resolutions", base(map[string]any{"resolutions": nil})},

		{"streamtype-1", base(map[string]any{"streamtype": 1})},
		{"stream_type-1", base(map[string]any{"stream_type": 1})},
		{"streamindex-1", base(map[string]any{"streamindex": 1})},
		{"channels-1", base(map[string]any{"channels": []int{*ch + 1}})},
	}

	fmt.Printf("%-28s %-6s %8s %6s %8s %s\n", "CASE", "HTTP", "BYTES", "VIDPID", "KB/S", "NOTE/details")
	for _, tc := range cases {
		res := run(*addr, *user, enc, tc, time.Duration(*secs)*time.Second)
		note := res.note
		if res.jsonMsg != "" {
			note = strings.TrimSpace(note + " " + res.jsonMsg)
		}
		fmt.Printf("%-28s %-6s %8d %6d %8.1f %s\n", tc.name, res.http, res.bytes, res.vidPID, float64(res.bytes)/1024/secsF(*secs), trunc(note, 150))
		time.Sleep(500 * time.Millisecond) // 让 NVR 释放会话
	}
}

func secsF(s int) float64 { return float64(s) }

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type result struct {
	http    string
	bytes   int
	vidPID  int
	note    string
	jsonMsg string
}

// dumpW: 非空时把 video part 的原始 TS 写入该文件（-res 单跑模式）。
var dumpW io.Writer

// dumpRun: 单个 resolutions 值，可选把原始 TS dump 到文件。
func dumpRun(addr, user, enc string, ch int, res, out string, dur time.Duration) {
	tc := testCase{name: "dump-" + res, body: map[string]any{
		"channels": []int{ch}, "privary_auth": []int{0}, "resolutions": []string{res},
	}}
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		dumpW = f
	}
	r := run(addr, user, enc, tc, dur)
	fmt.Printf("%-16s HTTP=%s bytes=%d KB/s=%.1f pids=%s json=%s\n",
		res, r.http, r.bytes, float64(r.bytes)/1024/dur.Seconds(), r.note, r.jsonMsg)
}

// run: 一个用例 —— 建立会话，读 secs 秒，统计。
func run(addr, user, enc string, tc testCase, dur time.Duration) result {
	r := result{vidPID: -1}
	conn, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		r.http = "DIALERR"
		r.note = err.Error()
		return r
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	uuid := fmt.Sprintf("%032x", time.Now().UnixNano())
	post1 := fmt.Sprintf("POST /stream HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nContent-Type: multipart/mixed;boundary=--%s--\r\n\r\n",
		uuid, addr, streamBoundary)
	conn.Write([]byte(post1))

	wa, err := readHead(conn)
	if err != nil {
		r.http = "HEADERR"
		r.note = err.Error()
		return r
	}
	nonce, opaque := authParam(headerVal(wa, "www-authenticate"), "nonce"), authParam(headerVal(wa, "www-authenticate"), "opaque")

	reqJSON, _ := json.Marshal(map[string]any{
		"type": "request", "seq": 0,
		"params": map[string]any{"method": "get", "preview": tc.body},
	})
	var body bytes.Buffer
	body.WriteString("----" + streamBoundary + "--\r\n")
	body.WriteString("Content-Type: application/json\r\n")
	body.WriteString("X-Data-Window-Size: 50\r\n")
	fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(reqJSON))
	body.Write(reqJSON)
	body.WriteString("\r\n")
	for i, rc := 0, 25; i < 10; i, rc = i+1, rc+25 {
		nj, _ := json.Marshal(map[string]any{"type": "notification", "params": map[string]any{"event_type": "stream_sequence"}})
		body.WriteString("----" + streamBoundary + "--\r\n")
		body.WriteString("X-Session-Id: 1\r\nContent-Type: application/json\r\n")
		fmt.Fprintf(&body, "X-Data-Received: %d\r\n", rc)
		fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(nj))
		body.Write(nj)
		body.WriteString("\r\n")
	}
	auth := digestResp(nonce, opaque, user, enc)
	post2 := fmt.Sprintf("POST /stream HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: multipart/mixed;boundary=--%s--\r\n\r\n",
		uuid, addr, auth, streamBoundary)
	full := append([]byte(post2), body.Bytes()...)
	conn.Write(full)

	status, err := readHead(conn)
	if err != nil {
		r.http = "HEAD2ERR"
		r.note = err.Error()
		return r
	}
	if len(status) > 0 {
		// "HTTP/1.0 200 OK" → 取 code
		f := strings.Fields(status[0])
		if len(f) >= 2 {
			r.http = f[1]
		} else {
			r.http = status[0]
		}
	}
	if !strings.Contains(status[0], "200") {
		r.note = strings.Join(status, " | ")
		return r
	}
	conn.SetDeadline(time.Now().Add(20 * time.Second))

	// 读 dur 秒，解析 multipart，统计。
	sep := []byte("\r\n----device-stream-boundary--")
	buf := make([]byte, 1<<16)
	acc := []byte{}
	deadline := time.Now().Add(dur)
	pids := map[int]int{}
	var pmtDesc []string
	session := "1"
	credit := 275
	lastAck := time.Now()
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			r.bytes += n
			acc = append(acc, buf[:n]...)
			for {
				idx := bytes.Index(acc, sep)
				if idx < 0 {
					break
				}
				part := acc[:idx]
				acc = acc[idx+len(sep):]
				head, bp, _ := bytes.Cut(part, []byte("\r\n\r\n"))
				if s := sessID(bp); s != "" {
					session = s
				}
				if bytes.Contains(head, []byte("application/json")) {
					r.jsonMsg = trunc(string(bp), 120)
				}
				if bytes.Contains(head, []byte("video/mp2t")) {
					countPIDs(bp, pids, &r.vidPID, &pmtDesc)
					if dumpW != nil {
						dumpW.Write(bp)
					}
				}
			}
		}
		if time.Since(lastAck) > 2*time.Second {
			credit += 25
			nj := []byte(`{"type":"notification","params":{"event_type":"stream_sequence"}}`)
			ack := fmt.Sprintf("\r\n----%s--\r\nX-Session-Id: %s\r\nContent-Type: application/json\r\nX-Data-Received: %d\r\nContent-Length: %d\r\n\r\n%s\r\n",
				streamBoundary, session, credit, len(nj), nj)
			conn.Write([]byte(ack))
			lastAck = time.Now()
		}
		if err != nil {
			break
		}
	}
	// 汇总 PID 分布
	var sb strings.Builder
	for pid, c := range pids {
		fmt.Fprintf(&sb, "pid%d=%d ", pid, c)
	}
	r.note = strings.TrimSpace(sb.String() + " " + strings.Join(pmtDesc, ";"))
	return r
}

func countPIDs(b []byte, pids map[int]int, vidPID *int, pmtDesc *[]string) {
	for off := 0; off+188 <= len(b); off += 188 {
		if b[off] != 0x47 {
			continue
		}
		pid := (int(b[off+1]&0x1f) << 8) | int(b[off+2])
		pids[pid]++
		if pid == 0x11 { // SDT
			continue
		}
	}
}

func sessID(b []byte) string {
	var m struct {
		Params struct {
			SessionID string `json:"session_id"`
		} `json:"params"`
	}
	if json.Unmarshal(b, &m) == nil {
		return m.Params.SessionID
	}
	return ""
}

func readHead(conn net.Conn) ([]string, error) {
	br := make([]byte, 0, 1024)
	one := make([]byte, 1)
	var lines []string
	for {
		n, err := conn.Read(one)
		if n > 0 {
			br = append(br, one[0])
			if len(br) >= 2 && br[len(br)-2] == '\r' && br[len(br)-1] == '\n' {
				line := strings.TrimRight(string(br), "\r\n")
				if line == "" {
					return lines, nil
				}
				lines = append(lines, line)
				br = br[:0]
			}
			if len(br) > 8192 {
				return lines, fmt.Errorf("header too long")
			}
		}
		if err != nil {
			return lines, err
		}
	}
}

func headerVal(lines []string, key string) string {
	for _, l := range lines {
		if i := strings.Index(l, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(l[:i]), key) {
			return strings.TrimSpace(l[i+1:])
		}
	}
	return ""
}

func authParam(wa, key string) string {
	if wa == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(wa), "digest") {
		wa = wa[len("digest"):]
	}
	for _, part := range strings.Split(wa, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), key) {
			return strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	return ""
}

var _ = io.EOF
