// Package tplink: 与 TP-Link NVR 8000 流服务器通信的最小协议实现。
//
// 只实现实时预览（preview）所需的子集：
//   - HTTP Digest 认证（凭据 = securityEncode(密码)，Digest 值必须带引号）
//   - 两步 POST /stream 握手（Content-Length: -1，需原生 TCP）
//   - multipart/mixed JSON 信封 + 信用流控（X-Data-Window-Size / stream_sequence）
//   - 响应 multipart 解析（video/mp2t part + JSON part），TS 清洗（见 tsclean.go）
//
// 协议细节见仓库根 PROTOCOL.md。
package tplink

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
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

// tpKey: securityEncode 用的大 KEY 串（与 TP-Link 经典算法一致，已对 TL-NVR6108C-L 验证）
const tpKey = "yLwVl0zKqws7LgKPRQ84Mdt708T1qQ3Ha7xv3H7NyU84p21BriUWBU43odz3iP4rBL3cD02KZci" +
	"XTysVXiV8ngg6vL48rPJyAUw0HurW20xqxv9aYb4M9wK1Ae0wlro510qXeU07kV57fQMc8L6aLgMLwygtc0F10a0Dg70TOoouy" +
	"FhdysuRMO51yY5ZlOZZLEal1h0t9YQW0Ko7oBwmCAHoic4HYbUyVeU3sfQ1xtXcPcf1aT303wAQhv66qzW"

// Client: 一台 NVR 的连接凭据与地址。
type Client struct {
	Addr   string // "host:port"（如 "192.168.0.49:8000"）
	User   string
	encPw  string // securityEncode 后的密码
	dialer net.Dialer
}

// New: 创建客户端。password 是 NVR 管理员明文密码，内部会做 securityEncode。
func New(addr, user, password string) *Client {
	return &Client{
		Addr:  addr,
		User:  user,
		encPw: securityEncode(password),
		dialer: net.Dialer{
			Timeout: 8 * time.Second,
		},
	}
}

// securityEncode: TP-Link 经典密码混淆（同时是 /ds 登录密码与 8000 Digest 密码）。
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

func randHexN(n int) string {
	b := make([]byte, n)
	if f, err := os.Open("/dev/urandom"); err == nil {
		_, _ = f.Read(b)
		_ = f.Close()
	}
	return hex.EncodeToString(b)
}

// digestResp: 计算 Digest response（qop=auth，uri=/stream）。值全部带引号 —— NVR 必需。
func digestResp(nonce, opaque, user, pw string) string {
	ha1 := md5hex(user + ":" + nvrRealm + ":" + pw)
	ha2 := md5hex("POST:/stream")
	cn := "0a1b2c3d"
	nc := "00000001"
	r := md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cn + ":auth:" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="/stream", qop=auth, nc=%s, cnonce="%s", response="%s", opaque="%s"`,
		user, nvrRealm, nonce, nc, cn, r, opaque)
}

// nvrConn: 一条已建立（带 Digest 会话）的 8000 连接
type nvrConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialStream: 与 NVR 建立 preview 会话并发送消息（App 同款）。
func (c *Client) dialStream(ch int) (*nvrConn, error) {
	conn, err := c.dialer.Dial("tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	// POST #1: 无认证
	post1 := fmt.Sprintf("POST /stream HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nContent-Type: multipart/mixed;boundary=--client-stream-boundary--\r\n\r\n",
		randHexN(16), c.Addr)
	if _, err := conn.Write([]byte(post1)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	head, err := readHTTPHead(br)
	if err != nil {
		conn.Close()
		return nil, err
	}
	wa := headerVal(head, "www-authenticate")
	nonce := authParam(wa, "nonce")
	opaque := authParam(wa, "opaque")
	// POST #2: 认证 + body（请求 + 预发信用确认，无闭合分隔符）
	reqJSON, _ := json.Marshal(map[string]any{
		"type": "request", "seq": 0,
		"params": map[string]any{"method": "get", "preview": map[string]any{
			"channels":     []int{ch},
			"privary_auth": []int{0},
			"resolutions":  []string{"HD"},
		}},
	})
	var body bytes.Buffer
	body.WriteString("----" + streamBoundary + "--\r\n")
	body.WriteString("Content-Type: application/json\r\n")
	body.WriteString("X-Data-Window-Size: 50\r\n")
	fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(reqJSON))
	body.Write(reqJSON)
	body.WriteString("\r\n")
	for i, rc := 0, 25; i < 10; i, rc = i+1, rc+25 { // 预发 25..250
		nj, _ := json.Marshal(map[string]any{"type": "notification", "params": map[string]any{"event_type": "stream_sequence"}})
		body.WriteString("----" + streamBoundary + "--\r\n")
		body.WriteString("X-Session-Id: 1\r\nContent-Type: application/json\r\n")
		fmt.Fprintf(&body, "X-Data-Received: %d\r\n", rc)
		fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(nj))
		body.Write(nj)
		body.WriteString("\r\n")
	}
	authHdr := digestResp(nonce, opaque, c.User, c.encPw)
	post2 := fmt.Sprintf("POST /stream HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: multipart/mixed;boundary=--client-stream-boundary--\r\n\r\n",
		randHexN(16), c.Addr, authHdr)
	full := append([]byte(post2), body.Bytes()...)
	if _, err := conn.Write(full); err != nil {
		conn.Close()
		return nil, err
	}
	head2, err := readHTTPHead(br)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(head2[0], "200") {
		conn.Close()
		return nil, fmt.Errorf("HTTP %s", head2[0])
	}
	conn.SetDeadline(time.Time{})
	return &nvrConn{conn: conn, br: br}, nil
}

// Stream: 实时预览通道 ch 的 TS 流 + 自动重连。
// clean=true 时做 TS 清洗（剔除 TP-Link 私有流 0x92，只留视频流）；
// clean=false 原样透传 NVR 原始流。
// 返回的 io.ReadCloser 由调用方负责 Close；读失败内部自动重连。
func (c *Client) Stream(ch int, clean bool) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			cn, err := c.dialStream(ch)
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}
			if err := c.streamLoop(cn, pw, clean); err != nil {
				cn.conn.Close()
			}
			cn.conn.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	return pr
}

// streamLoop: 读 multipart 响应，透出 video/mp2t part（可选清洗），并持续回发信用确认。
func (c *Client) streamLoop(cn *nvrConn, w io.Writer, clean bool) error {
	sessionID := "1"
	credit := 275
	lastAck := time.Now()
	sep := []byte("\r\n----device-stream-boundary--")
	var cleaner *tsCleaner
	if clean {
		cleaner = newTSCleaner()
	}
	buf := make([]byte, 1<<16)
	acc := []byte{}
	deadline := time.Now().Add(30 * time.Second) // 无数据超时则重连
	cn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, errr := cn.br.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			for {
				idx := bytes.Index(acc, sep)
				if idx < 0 {
					break
				}
				part := acc[:idx]
				acc = acc[idx+len(sep):]
				h, bodyPart, _ := bytes.Cut(part, []byte("\r\n\r\n"))
				if sess := regexpSession(bodyPart); sess != "" {
					sessionID = sess
				}
				if bytes.Contains(h, []byte("video/mp2t")) && len(bodyPart) > 0 {
					if clean {
						bodyPart = cleaner.feed(bodyPart)
					}
					if len(bodyPart) > 0 {
						if _, e := w.Write(bodyPart); e != nil {
							return nil // 下游断开, 结束本次循环
						}
					}
				}
			}
			deadline = time.Now().Add(30 * time.Second)
		}
		// 信用确认（2s 一次，+25）
		if time.Since(lastAck) > 2*time.Second {
			credit += 25
			nj, _ := json.Marshal(map[string]any{"type": "notification", "params": map[string]any{"event_type": "stream_sequence"}})
			ack := fmt.Sprintf("\r\n----%s--\r\nX-Session-Id: %s\r\nContent-Type: application/json\r\nX-Data-Received: %d\r\nContent-Length: %d\r\n\r\n%s\r\n",
				streamBoundary, sessionID, credit, len(nj), nj)
			cn.conn.Write([]byte(ack))
			lastAck = time.Now()
		}
		if errr != nil {
			if ne, ok := errr.(net.Error); ok && ne.Timeout() {
				cn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if time.Now().After(deadline) {
					return nil
				}
				continue
			}
			return nil
		}
		cn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}
}

// regexpSession: 从 JSON part 提取 session_id
func regexpSession(b []byte) string {
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

func readHTTPHead(br *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func authParam(wa, key string) string {
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

func headerVal(lines []string, key string) string {
	for _, l := range lines {
		if i := strings.Index(l, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(l[:i]), key) {
			return strings.TrimSpace(l[i+1:])
		}
	}
	return ""
}