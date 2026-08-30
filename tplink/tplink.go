// Package tplink: 与 TP-Link NVR 8000 流服务器通信的最小协议实现。
//
// 只实现实时预览（preview）所需的子集：
//   - HTTP Digest 认证（凭据 = securityEncode(密码)，Digest 值必须带引号）
//   - 两步 POST /stream 握手（Content-Length: -1，需原生 TCP）
//   - multipart/mixed JSON 信封 + 信用流控（X-Data-Window-Size / stream_sequence）
//   - 响应 multipart 解析（video/mp2t part + JSON part），TS 清洗（见 tsclean.go）
//   - 每通道共享会话扇出：同通道多个订阅者只占一路 NVR 会话，各自独立下行缓冲
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
	"sync"
	"time"
)

const (
	streamBoundary = "client-stream-boundary"
	nvrRealm       = "TP-Link IP-Camera"
	tpSalt         = "RDpbLfCPsJZ7fiv"

	// hubIdleGrace: 最后一个订阅者离开后，NVR 会话再保留的时长；
	// 期间有新的订阅者到达则直接复用，否则关闭会话回到待命。
	hubIdleGrace = 10 * time.Second
	// subBufBytes: 每个订阅者下行缓冲上限（字节）。慢客户端溢出时丢最旧，
	// 只影响自己，不拖累其他订阅者。
	subBufBytes = 256 << 10
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

	mu   sync.Mutex
	hubs map[hubKey]*hub // 每 (通道, 清洗形态) 一个共享扇出 hub；首个订阅者触发建连，最后一个离开延迟关闭
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
		hubs: map[hubKey]*hub{},
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

// Stream: 订阅 NVR API 通道 ch 的实时预览流（读失败内部自动重连）。
// 注意 ch 是 NVR 协议通道号（0 起）；HTTP 端点的用户通道号（1 起）已在 main.go 换算。
// 同一 (通道, clean) 的多个订阅者共享一路 NVR 会话：上游只拉一次，
// 清洗（如果需要）在 hub 侧只做一遍，再扇出给所有订阅者（各自独立下行缓冲，
// 满则丢最旧，只影响自己）。
// clean=true 时 hub 统一做 TS 清洗（剔除 TP-Link 私有流，只留视频流），
// 所有订阅者拿到同一份清洗后的流；clean=false 原样透传 NVR 原始流。
// 返回的 io.ReadCloser 由调用方负责 Close。
func (c *Client) Stream(ch int, clean bool) io.ReadCloser {
	key := hubKey{ch: ch, clean: clean}
	c.mu.Lock()
	h := c.hubs[key]
	if h == nil {
		h = &hub{ch: ch, clean: clean, c: c, subs: map[*sub]struct{}{}, wake: make(chan struct{}, 1)}
		if clean {
			h.cleaner = newTSCleaner()
		}
		c.hubs[key] = h
		go h.run()
	}
	c.mu.Unlock()
	return h.subscribe()
}

// hubKey: 通道与会话形态（清洗与否）一起作为共享上游的键：需要原始流的
// 订阅者和需要清洗流的订阅者各自一条上游，互不干扰。
type hubKey struct {
	ch    int
	clean bool
}

// hub: 一个 (通道, 清洗形态) 的共享上游。多个 HTTP 客户端订阅同一 key 时，
// 只维持一路 NVR preview 会话，hub 把流扇出给所有订阅者；hub 与上游循环
// 常驻进程生命周期，无订阅者时待命（不占 NVR 会话）。
// clean 形态下清洗在 hub 做一遍（cleaner 常驻，学习到的 PID 跨会话保留），
// 订阅者拿到的是同一份已清洗的流。
type hub struct {
	ch      int
	clean   bool
	cleaner *tsCleaner // clean 时非空：feed 前先清洗一次
	c       *Client
	mu      sync.Mutex
	subs    map[*sub]struct{}
	wake    chan struct{} // 缓冲 1：有订阅者到达时唤醒上游循环
}

// sub: 一个订阅者（对应一个 HTTP 客户端）。实现 io.ReadCloser：
// Read 阻塞取流（可跨 TS 包边界返回任意长度），Close 取消订阅。
type sub struct {
	h    *hub
	ring *tsRing
	once sync.Once
}

// subscribe: 注册一个新订阅者并唤醒上游循环。
func (h *hub) subscribe() *sub {
	h.mu.Lock()
	s := &sub{h: h, ring: newTSRing(subBufBytes)}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return s
}

// run: 通道上游循环。有订阅者时维持一路 NVR preview 会话（断流自动重连）；
// 无订阅者时待命，等订阅者到来。
func (h *hub) run() {
	for {
		if h.subCount() == 0 {
			<-h.wake // 待命；可能消费到陈旧唤醒，回到循环再核对一次
			continue
		}
		cn, err := h.c.dialStream(h.ch)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		h.consume(cn)
		cn.conn.Close()
		time.Sleep(2 * time.Second)
	}
}

func (h *hub) subCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// consume: 读 multipart 响应，透出 video/mp2t part 并扇出给所有订阅者，
// 持续回发信用确认。无订阅者超过 hubIdleGrace 则结束本次会话（回待命）。
func (h *hub) consume(cn *nvrConn) error {
	sessionID := "1"
	credit := 275
	lastAck := time.Now()
	sep := []byte("\r\n----device-stream-boundary--")
	buf := make([]byte, 1<<16)
	acc := []byte{}
	deadline := time.Now().Add(30 * time.Second) // 无数据超时则重连
	cn.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if h.subCount() == 0 {
			// 最后订阅者已离开：留宽限窗口，期间有新订阅者则复用本会话
			select {
			case <-h.wake:
			case <-time.After(hubIdleGrace):
				return nil
			}
		}
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
				head, bodyPart, _ := bytes.Cut(part, []byte("\r\n\r\n"))
				if sess := regexpSession(bodyPart); sess != "" {
					sessionID = sess
				}
				if bytes.Contains(head, []byte("video/mp2t")) && len(bodyPart) > 0 {
					h.feed(bodyPart)
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

// feed: 上游的一个 video part → 先（按需）清洗一次，再扇出给所有订阅者。
// clean 形态下清洗只做一遍：所有订阅者共享同一份输出，
// 避免 N 个订阅者对同一份原始流重复清洗。
func (h *hub) feed(part []byte) {
	if h.clean {
		part = h.cleaner.feed(part)
	}
	h.broadcast(part)
}

// broadcast: 把一段（已按需清洗的）TS 数据扇出给当前所有订阅者。
// part 可能别名上游累计缓冲（下次读会复用），必须拷贝后再分发；
// 各订阅者共享同一份拷贝（只读，各自入队）。
func (h *hub) broadcast(part []byte) {
	h.mu.Lock()
	n := len(h.subs)
	if n == 0 {
		h.mu.Unlock()
		return
	}
	subs := make([]*sub, 0, n)
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	chunk := make([]byte, len(part))
	copy(chunk, part)
	for _, s := range subs {
		s.ring.push(chunk)
	}
}

// Read: 阻塞读取 TS 数据。Close 后返回 io.EOF。
func (s *sub) Read(p []byte) (int, error) { return s.ring.read(p) }

// Close: 取消订阅。hub 会保留当前 NVR 会话最多 hubIdleGrace，
// 期间新订阅者可直接复用。
func (s *sub) Close() error {
	s.once.Do(func() {
		s.h.mu.Lock()
		delete(s.h.subs, s)
		s.h.mu.Unlock()
		s.ring.close()
	})
	return nil
}

// tsRing: 订阅者下行缓冲。生产者（上游）push，消费者（HTTP 客户端）read。
// 超过 maxBytes 丢最旧；close 后 read 返回 EOF。并发安全。
type tsRing struct {
	mu       sync.Mutex
	cond     *sync.Cond
	data     [][]byte
	size     int
	maxBytes int
	closed   bool
}

func newTSRing(maxBytes int) *tsRing {
	r := &tsRing{maxBytes: maxBytes}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *tsRing) push(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.data = append(r.data, chunk)
	r.size += len(chunk)
	for r.size > r.maxBytes && len(r.data) > 1 {
		r.size -= len(r.data[0])
		r.data = r.data[1:]
	}
	r.cond.Signal()
}

func (r *tsRing) read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.data) == 0 && !r.closed {
		r.cond.Wait()
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	total := 0
	for total < len(p) && len(r.data) > 0 {
		chunk := r.data[0]
		n := copy(p[total:], chunk)
		total += n
		if n == len(chunk) {
			r.data = r.data[1:]
		} else {
			r.data[0] = chunk[n:]
			break
		}
	}
	r.size -= total
	return total, nil
}

func (r *tsRing) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cond.Broadcast()
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
