package tplink

import (
	"bytes"
	"io"
	"reflect"
	"testing"
	"time"
)

// ---- tsRing ----

func TestTSRingBasic(t *testing.T) {
	r := newTSRing(1 << 20)
	r.push([]byte("hello "))
	r.push([]byte("world"))

	buf := make([]byte, 8)
	n, err := r.read(buf)
	if err != nil || n != 8 || string(buf[:n]) != "hello wo" {
		t.Fatalf("got %d %q err=%v", n, buf[:n], err)
	}
	rest := make([]byte, 64)
	n, err = r.read(rest)
	if err != nil || string(rest[:n]) != "rld" {
		t.Fatalf("got %d %q err=%v", n, rest[:n], err)
	}
	// 再读应阻塞（无数据）；Close 后 EOF
	closed := make(chan error, 1)
	go func() {
		_, err := r.read(rest)
		closed <- err
	}()
	r.close()
	if err := <-closed; err != io.EOF {
		t.Fatalf("expected EOF after close, got %v", err)
	}
}

func TestTSRingOverflowDropsOldest(t *testing.T) {
	r := newTSRing(10)     // 10 字节上限
	r.push([]byte("aaaa")) // 4
	r.push([]byte("bbbb")) // 8
	r.push([]byte("cc"))   // 10
	r.push([]byte("dddd")) // 超 → 丢 "aaaa"，剩 bb+cc+dddd=10
	buf := make([]byte, 128)
	n, err := r.read(buf)
	if err != nil || string(buf[:n]) != "bbbbccdddd" {
		t.Fatalf("got %d %q err=%v", n, buf[:n], err)
	}
	// 读尽后空 → 阻塞等待；Close 后 EOF
	r.close()
	if _, err := r.read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestTSRingCloseUnblocksReader(t *testing.T) {
	r := newTSRing(1 << 20)
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := r.read(buf)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	r.close()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("expected EOF, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read not unblocked by close")
	}
	// close 后 push 被忽略
	r.push([]byte("x"))
	if r.size != 0 || len(r.data) != 0 {
		t.Fatal("push after close must be ignored")
	}
}

// ---- hub 订阅/扇出 ----

func newTestHub(clean bool) *hub {
	h := &hub{clean: clean, c: &Client{}, subs: map[*sub]struct{}{}, wake: make(chan struct{}, 1)}
	if clean {
		h.cleaner = newTSCleaner()
	}
	return h
}

func TestHubSubscribeCloseCounts(t *testing.T) {
	h := newTestHub(false)
	if h.subCount() != 0 {
		t.Fatal("fresh hub must have 0 subs")
	}
	s1 := h.subscribe()
	s2 := h.subscribe()
	if h.subCount() != 2 {
		t.Fatalf("want 2 subs, got %d", h.subCount())
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if h.subCount() != 1 {
		t.Fatalf("want 1 sub, got %d", h.subCount())
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if h.subCount() != 0 {
		t.Fatalf("want 0 subs, got %d", h.subCount())
	}
	// 重复 Close 幂等
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if h.subCount() != 0 {
		t.Fatalf("double close must not double-unsubscribe, got %d", h.subCount())
	}
}

// TestHubBroadcastCopies: broadcast 必须拷贝 bodyPart（它别名上游累计缓冲，
// 之后会被覆盖）；多个订阅者各自拿到独立数据。
func TestHubBroadcastCopies(t *testing.T) {
	h := newTestHub(false)
	s1 := h.subscribe()
	s2 := h.subscribe()

	src := []byte("0123456789abcdef")
	h.broadcast(src)
	// 模拟上游缓冲复用：覆盖源数据
	for i := range src {
		src[i] = 'X'
	}

	for i, s := range []*sub{s1, s2} {
		buf := make([]byte, 128)
		n, err := s.Read(buf)
		if err != nil || string(buf[:n]) != "0123456789abcdef" {
			t.Fatalf("sub %d got %q err=%v", i, buf[:n], err)
		}
	}
}

func TestHubBroadcastNoSubs(t *testing.T) {
	h := newTestHub(false)
	h.broadcast([]byte("data")) // 不应 panic / 阻塞
}

// TestHubCloseStopsDelivery: 订阅者 Close 后不再收到新数据（push 被忽略），
// 缓冲读尽后返回 EOF。
func TestHubCloseStopsDelivery(t *testing.T) {
	h := newTestHub(false)
	s := h.subscribe()
	h.broadcast([]byte("first"))

	buf := make([]byte, 64)
	if n, _ := s.Read(buf); string(buf[:n]) != "first" {
		t.Fatalf("want buffered 'first', got %q", buf[:n])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.broadcast([]byte("second")) // close 后 push 应被 ring 忽略

	n, err := s.Read(buf)
	if err != io.EOF {
		t.Fatalf("closed sub must EOF, got %q err=%v", buf[:n], err)
	}
}

// TestHubFeedCleanOnce: clean hub 在 feed 时清洗一次，所有订阅者拿到
// 同一份清洗后的流（无 0x192 私有流），而不是各自重复清洗。
func TestHubFeedCleanOnce(t *testing.T) {
	h := newTestHub(true)
	s1 := h.subscribe()
	s2 := h.subscribe()

	stream := buildTestTS()
	h.feed(stream)

	buf := make([]byte, 8192)
	n1, err := s1.Read(buf)
	if err != nil || n1 == 0 {
		t.Fatalf("sub1 read: n=%d err=%v", n1, err)
	}
	c1 := buf[:n1]
	n2, err := s2.Read(buf)
	if err != nil || n2 == 0 {
		t.Fatalf("sub2 read: n=%d err=%v", n2, err)
	}
	c2 := buf[:n2]

	if !bytes.Equal(c1, c2) {
		t.Fatal("clean hub subscribers must receive the identical cleaned stream")
	}
	if len(c1) == 0 || len(c1) >= len(stream) {
		t.Fatalf("cleaned stream should be smaller (private dropped): raw=%d clean=%d", len(stream), len(c1))
	}
	for off := 0; off+188 <= len(c1); off += 188 {
		if c1[off] != 0x47 {
			t.Fatalf("cleaned stream lost TS sync at %d", off)
		}
		pid := (int(c1[off+1]&0x1f) << 8) | int(c1[off+2])
		if pid == 0x192 {
			t.Fatalf("cleaned stream still contains private PID 0x192 at %d", off)
		}
	}
}

// TestHubFeedRaw: raw hub 的 feed 原样透传。
func TestHubFeedRaw(t *testing.T) {
	h := newTestHub(false)
	s := h.subscribe()

	stream := buildTestTS()
	h.feed(stream)

	buf := make([]byte, 8192)
	n, err := s.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf[:n], stream) {
		t.Fatal("raw hub feed must pass through the exact original bytes")
	}
}

// ---- 码流档位 ----

// TestResWireValues: 线上值是 NVR 固件子串匹配用的字符串，固定为 HD/VGA；
// String() 返回语义名 main/sub（避免日志里出现易误读的 "VGA"）。
func TestResWireValues(t *testing.T) {
	if ResMain != "HD" {
		t.Fatalf("主码流线上值应为 HD, got %q", ResMain)
	}
	if ResSub != "VGA" {
		t.Fatalf("子码流线上值应为 VGA, got %q", ResSub)
	}
	if ResMain.String() != "main" || ResSub.String() != "sub" {
		t.Fatalf("String() 应返回 main/sub, got %q/%q", ResMain, ResSub)
	}
}

// TestHubKeyDistinguishesRes: 同一通道的不同档位必须是各自独立的 hub key，
// 否则主/子码流会互相串流（子码流订阅者拿到高清流）。
func TestHubKeyDistinguishesRes(t *testing.T) {
	mainKey := hubKey{ch: 0, res: ResMain, clean: false}
	subKey := hubKey{ch: 0, res: ResSub, clean: false}
	if mainKey == subKey {
		t.Fatal("不同档位必须映射到不同 hub key")
	}
	// 档位相同、清洗形态不同也必须区分（原有行为）
	if mainKey == (hubKey{ch: 0, res: ResMain, clean: true}) {
		t.Fatal("clean 形态必须参与 hub key")
	}
	// 通道不同必须区分
	if mainKey == (hubKey{ch: 1, res: ResMain, clean: false}) {
		t.Fatal("通道号必须参与 hub key")
	}
}

// TestBuildPreviewResolutions: preview 请求体必须带上档位对应的 resolutions，
// 且空档位回落主码流(HD)；extra 可覆盖。
func TestBuildPreviewResolutions(t *testing.T) {
	get := func(res []string, extra map[string]any) map[string]any {
		return buildPreview(3, res, extra)
	}
	// 默认(空) → HD 主码流
	if got := get(nil, nil)["resolutions"]; !reflect.DeepEqual(got, []string{"HD"}) {
		t.Fatalf("空档位应回落 HD, got %v", got)
	}
	// 子码流 → VGA
	if got := get([]string{string(ResSub)}, nil)["resolutions"]; !reflect.DeepEqual(got, []string{"VGA"}) {
		t.Fatalf("子码流应发 VGA, got %v", got)
	}
	// 通道号正确落到 channels
	if got := get(nil, nil)["channels"]; !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("channels 应为 [3], got %v", got)
	}
	// 固定字段 pri 认证存在
	if got := get(nil, nil)["privary_auth"]; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("privary_auth 应为 [0], got %v", got)
	}
	// extra 覆盖 resolutions
	if got := get(nil, map[string]any{"resolutions": []string{"SVGA"}})["resolutions"]; !reflect.DeepEqual(got, []string{"SVGA"}) {
		t.Fatalf("extra 应覆盖 resolutions, got %v", got)
	}
}

// buildTestTS: 构造一帧最小可清洗的 TS：PAT + PMT(HEVC 0x101) + 视频 + 私有流 0x192
func buildTestTS() []byte {
	var ts []byte
	// PAT 包: pid=0, section: table 0x00, slen=13
	pat := tsPkt(0, []byte{
		0xB0, 0x0D,
		0x00, 0x01, // transport_stream_id
		0x01, 0x00, 0x00, // version/cur, sec, last
		0x00, 0x01, 0xE1, 0x00, // program 1 → PMT pid 0x100
		0x01, 0x02, 0x03, 0x04, // CRC（解析器不校验）
	})
	ts = append(ts, pat...)
	// PMT 包: pid=0x100, slen=18, program_info_length=0, ES: 0x24 HEVC pid 0x101
	pmt := tsPkt(0x100, []byte{
		0xB0, 0x12,
		0x00, 0x01, // program_number
		0x01, 0x00, 0x00, // version/cur, sec, last
		0xE1, 0x01, // PCR pid 0x101
		0x00, 0x00, // program_info_length = 0
		0x24, 0xE1, 0x01, 0x00, 0x00, // stream_type HEVC, ES pid 0x101, info len 0
		0xDE, 0xAD, 0xBE, 0xEF, // CRC
	})
	ts = append(ts, pmt...)
	vid := make([]byte, 188)
	vid[0] = 0x47
	vid[1] = 0x41 // pid 0x101 | payload_unit_start
	vid[2] = 0x01
	vid[3] = 0x01
	ts = append(ts, vid...)
	for i := 0; i < 3; i++ {
		priv := make([]byte, 188)
		priv[0] = 0x47
		priv[1] = 0x21 // pid 0x192
		priv[2] = 0x92
		priv[3] = 0x01
		ts = append(ts, priv...)
	}
	return ts
}

// tsPkt: 构造 188 字节 TS 包（PAT/PMT 用），payload 从偏移 5 起（pointer_field=0，
// 适配 tsSection 解析），不足部分 0xFF 填充。
func tsPkt(pid int, payload []byte) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte(pid>>8) | 0x40 | 0x20
	pkt[2] = byte(pid)
	pkt[3] = 0x01 // 纯载荷
	pkt[4] = 0    // pointer_field
	copy(pkt[5:], payload)
	for i := 5 + len(payload); i < 188; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}
