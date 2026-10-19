package tplink

import (
	"bytes"
	"encoding/binary"
)

// tsRemuxer: 把 NVR 的私有 TS 重封装成下游能直接识别的标准 A/V TS。
//
// NVR preview 流的 PMT 里声明了两路 ES:
//
//	0x1b/0x24  视频 (H.264/HEVC)
//	0x92       TP-Link 私有流 —— 内容不是 0xd5 填充，而是每包一个完整的
//	           RIFF/WAVE 容器，fmt=6 (A-law)、单声道、8000Hz；WAVE 里的
//	           data 块就是 PCMA 音频。
//
// ffmpeg 的 MPEG-TS 解复用器不认识 0x92；go2rtc 的原生 mpegts 解复用器
// 虽然支持 0x90 (PCMA)，但只认 PMT 里声明的 PID。所以这里把 0x92 的
// WAVE 头剥掉，重封装成标准 PCMA PES (stream_id 0xC0, stream_type 0x90,
// PMT 里对应 ES 改成 0x90)，下游就能看到一条正常的 8kHz 单声道音频轨。
//
// clean=true 时额外丢掉 null/未知 PID（只保留 PAT/PMT/视频/音频）；
// clean=false 时未知 PID 仍透传。两种模式都会转换私有音频。
type tsRemuxer struct {
	clean bool

	buf []byte // <188 字节的残留

	pmtPID     int    // 从 PAT 学; -1=未知
	videoPID   int    // 从 PMT 学; -1=未知
	audioPID   int    // 从 PMT 的 0x92 ES 学; -1=未知
	pmtSection []byte // 重写后的 PMT section(含 CRC)；nil=不改写

	audioBuf []byte // 私有音频 PES 重组缓冲
	audioCC  int    // 输出音频 PID 的 continuity counter
	audioPTS uint64 // 原始 PES 没有 PTS 时的回退时间戳(90kHz)
}

func newTSRemuxer(clean bool) *tsRemuxer {
	return &tsRemuxer{clean: clean, pmtPID: -1, videoPID: -1, audioPID: -1}
}

// feed: 输入任意长度的 TS 字节流(可跨 part 边界), 返回重封装后的标准 TS 字节。
func (r *tsRemuxer) feed(in []byte) []byte {
	r.buf = append(r.buf, in...)
	var out []byte
	n := len(r.buf)
	start := 0
	for start+tsPacketSize <= n {
		if r.buf[start] != 0x47 {
			// 丢开对齐前, 找下一个 0x47 作为包边界(最多往后扫 188*2 字节)
			lim := start + tsPacketSize*2
			if lim > n {
				lim = n
			}
			j := bytes.IndexByte(r.buf[start+1:lim], 0x47)
			if j < 0 {
				break
			}
			start += 1 + j
			if start+tsPacketSize > n {
				break
			}
		}
		pkt := r.buf[start : start+tsPacketSize]
		pid := (int(pkt[1]&0x1f) << 8) | int(pkt[2])
		switch {
		case pid == 0: // PAT: 学 PMT PID 并透传
			r.learnPAT(pkt)
			out = append(out, pkt...)
		case r.pmtPID >= 0 && pid == r.pmtPID:
			if sec := extractPMTSection(pkt); sec != nil {
				r.learnPMT(sec)
			}
			if r.pmtSection != nil {
				out = append(out, rewritePMTPkt(pkt, r.pmtSection)...)
			} else {
				out = append(out, pkt...)
			}
		case r.videoPID >= 0 && pid == r.videoPID:
			out = append(out, pkt...)
		case r.audioPID >= 0 && pid == r.audioPID:
			r.feedAudioPacket(pkt, &out)
		default: // 私有流/未知 PID/null
			if !r.clean {
				out = append(out, pkt...)
			}
		}
		start += tsPacketSize
	}
	r.buf = append(r.buf[:0], r.buf[start:]...)
	return out
}

const (
	tsPacketSize = 188

	streamTypeH264         = 0x1b
	streamTypeHEVC         = 0x24
	streamTypePrivateAudio = 0x92 // TP-Link 私有 WAVE/A-law
	streamTypePCMA         = 0x90 // 标准 G.711 A-law (go2rtc 原生 mpegts 支持)
)

// learnPAT: 解析 PAT(表 id 0x00) 找到 PMT PID
func (r *tsRemuxer) learnPAT(pkt []byte) {
	s := tsSection(pkt, 0x00)
	if s == nil || len(s) < 12 {
		return
	}
	slen := (int(s[1]&0x0f) << 8) | int(s[2])
	end := 3 + slen - 4
	if end > len(s) {
		end = len(s)
	}
	for pos := 8; pos+4 <= end; pos += 4 {
		pn := (int(s[pos]&0x1f) << 8) | int(s[pos+1])
		if pn != 0 {
			r.pmtPID = (int(s[pos+2]&0x1f) << 8) | int(s[pos+3])
		}
	}
}

// learnPMT: 解析 PMT，记录视频/私有音频 PID，并生成重写后的 PMT：
// 视频 ES 原样保留，0x92 改成 0x90(PCMA, 无 ES info)，其余按 clean 取舍。
func (r *tsRemuxer) learnPMT(sec []byte) {
	if len(sec) < 16 {
		return
	}
	body := sec[:len(sec)-4] // 去掉 CRC
	if len(body) < 12 {
		return
	}
	progInfoLen := (int(body[10]&0x0f) << 8) | int(body[11])
	pos := 12 + progInfoLen
	if pos > len(body) {
		return
	}
	head := body[:pos] // table id + section_length + program header + program_info
	var kept []byte
	haveVideo := false
	audioPID := -1
	for pos+5 <= len(body) {
		st := body[pos]
		epid := (int(body[pos+1]&0x1f) << 8) | int(body[pos+2])
		elen := (int(body[pos+3]&0x0f) << 8) | int(body[pos+4])
		if pos+5+elen > len(body) {
			break
		}
		entry := body[pos : pos+5+elen]
		switch {
		case st == streamTypeH264 || st == streamTypeHEVC:
			kept = append(kept, entry...)
			haveVideo = true
			r.videoPID = epid
		case st == streamTypePrivateAudio && audioPID < 0:
			// 同一 PID 改成标准 PCMA，ES info 丢弃（PCMA 不需要描述符）
			audioPID = epid
			kept = append(kept, streamTypePCMA, entry[1], entry[2], 0xF0, 0x00)
		default:
			if !r.clean {
				kept = append(kept, entry...)
			}
		}
		pos += 5 + elen
	}

	// 没有私有音频且是透传模式：不改 PMT，保持原始字节
	if audioPID < 0 && !r.clean {
		r.pmtSection = nil
		return
	}
	if audioPID >= 0 {
		r.audioPID = audioPID
	}
	if len(kept) == 0 && !haveVideo {
		r.pmtSection = nil
		return
	}

	nb := append([]byte{}, head...)
	nb = append(nb, kept...)
	newLen := len(nb) - 3 + 4 // section_length 语义: 其后到 CRC 结束
	if newLen > 0x3FD {
		r.pmtSection = nil
		return
	}
	nb[1] = 0x30 | byte((newLen>>8)&0x0f)
	nb[2] = byte(newLen & 0xff)
	crc := mpegCRC32(nb)
	nb = append(nb, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	r.pmtSection = nb
}

// feedAudioPacket: 把私有 PID 的 TS 包重组为 PES；收到完整 PES 就转成
// 标准 PCMA PES 输出到 out。
func (r *tsRemuxer) feedAudioPacket(pkt []byte, out *[]byte) {
	payload, pusi := tsPayload(pkt)
	if payload == nil {
		return
	}
	if pusi {
		// 上一包如果还没收全，丢弃；完整 PES 在下面的常规路径已经处理过。
		r.audioBuf = r.audioBuf[:0]
	} else if len(r.audioBuf) == 0 {
		return // 没等到起始包
	}
	r.audioBuf = append(r.audioBuf, payload...)
	r.flushAudio(out)
}

// flushAudio: 从 audioBuf 里切出完整 PES 并转换。
func (r *tsRemuxer) flushAudio(out *[]byte) {
	for len(r.audioBuf) >= 6 {
		plen := (int(r.audioBuf[4]) << 8) | int(r.audioBuf[5])
		if plen == 0 { // 无长度：等到下一个 PUSI 时丢弃半包
			return
		}
		total := 6 + plen
		if len(r.audioBuf) < total {
			return
		}
		pes := make([]byte, total)
		copy(pes, r.audioBuf[:total])
		r.audioBuf = append(r.audioBuf[:0], r.audioBuf[total:]...)
		data, pts, ok := parseAudioPES(pes)
		if !ok {
			continue
		}
		r.emitAudioPES(data, pts, out)
	}
}

// parseAudioPES: 从私有 PES 里取出 WAVE 的 A-law data 和 PTS。
func parseAudioPES(pes []byte) (data, pts []byte, ok bool) {
	if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return nil, nil, false
	}
	total := len(pes)
	if plen := (int(pes[4]) << 8) | int(pes[5]); plen != 0 {
		total = 6 + plen
		if len(pes) < total {
			return nil, nil, false
		}
	}
	hlen := int(pes[8])
	if 9+hlen > total {
		return nil, nil, false
	}
	if pes[7]&0x80 != 0 && hlen >= 5 {
		pts = pes[9:14]
	}
	data = wavALaw(pes[9+hlen : total])
	if data == nil {
		return nil, nil, false
	}
	return data, pts, true
}

// wavALaw: 解析 RIFF/WAVE 容器，返回 8kHz A-law data 块。
// 只接受 fmt=6(WAVE_FORMAT_ALAW)；其它格式返回 nil（不猜测）。
func wavALaw(b []byte) []byte {
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		return nil
	}
	pos := 12
	var data []byte
	format := -1
	for pos+8 <= len(b) {
		id := b[pos : pos+4]
		size := int(binary.LittleEndian.Uint32(b[pos+4:]))
		body := pos + 8
		if size < 0 || body > len(b) {
			break
		}
		end := body + size
		if end > len(b) {
			end = len(b)
		}
		switch string(id) {
		case "fmt ":
			if size >= 16 && body+16 <= len(b) {
				format = int(binary.LittleEndian.Uint16(b[body:]))
			}
		case "data":
			data = b[body:end]
		}
		pos = end + (size & 1)
	}
	if format != 6 || data == nil {
		return nil
	}
	return data
}

// emitAudioPES: 把一段 A-law 数据封装成 PCMA PES 并写入 TS 包。
func (r *tsRemuxer) emitAudioPES(data, pts []byte, out *[]byte) {
	if len(data) == 0 {
		return
	}
	if len(data) > 65527 { // PES_packet_length 上限 65535，减去 8 字节头
		data = data[:65527]
	}
	length := 8 + len(data)
	pes := make([]byte, 0, 6+length)
	pes = append(pes, 0x00, 0x00, 0x01, 0xC0, byte(length>>8), byte(length), 0x80, 0x80, 0x05)
	if len(pts) == 5 {
		pes = append(pes, pts...)
	} else {
		pes = append(pes, encodePTS(r.audioPTS)...)
		r.audioPTS += uint64(len(data)) * 90000 / 8000
	}
	pes = append(pes, data...)
	writePESPackets(r.audioPID, &r.audioCC, pes, out)
}

// encodePTS: 5 字节 PTS (33-bit, 90kHz)
func encodePTS(pts uint64) []byte {
	return []byte{
		0x21 | byte((pts>>29)&0x0E),
		byte(pts >> 22),
		0x01 | byte((pts>>14)&0xFE),
		byte(pts >> 7),
		0x01 | byte((pts<<1)&0xFE),
	}
}

// writePESPackets: 把一个 PES 切成 188 字节 TS 包。最后一包用 adaptation
// field 填充，避免把填充字节当成 PCMA 采样。
func writePESPackets(pid int, cc *int, pes []byte, out *[]byte) {
	first := true
	for len(pes) > 0 {
		pkt := make([]byte, tsPacketSize)
		pkt[0] = 0x47
		pkt[1] = byte((pid >> 8) & 0x1f)
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
		}
		pkt[2] = byte(pid)
		if len(pes) >= 184 {
			pkt[3] = 0x10 | byte(*cc&0x0f)
			copy(pkt[4:], pes[:184])
			pes = pes[184:]
		} else {
			afl := 183 - len(pes)
			pkt[3] = 0x30 | byte(*cc&0x0f)
			pkt[4] = byte(afl)
			for i := 5; i < 5+afl; i++ {
				if i == 5 {
					pkt[i] = 0x00 // adaptation field flags
				} else {
					pkt[i] = 0xFF // stuffing
				}
			}
			copy(pkt[5+afl:], pes)
			pes = nil
		}
		*cc = (*cc + 1) & 0x0f
		first = false
		*out = append(*out, pkt...)
	}
}

// tsPayload: 返回 TS 包的载荷与 PUSI；无载荷返回 nil。
func tsPayload(pkt []byte) ([]byte, bool) {
	afc := (pkt[3] >> 4) & 0x03
	if afc == 0 || afc == 2 {
		return nil, false
	}
	pos := 4
	if afc == 3 {
		pos += 1 + int(pkt[4])
	}
	if pos >= tsPacketSize {
		return nil, false
	}
	return pkt[pos:], pkt[1]&0x40 != 0
}

// tsSection: 从 TS 包提取完整 PSI section(含 CRC), 校验 table id; 跨包/不完整的返回 nil
func tsSection(pkt []byte, wantTable byte) []byte {
	afc := (pkt[3] >> 4) & 0x03
	if afc != 1 && afc != 3 {
		return nil
	}
	po := 4
	if afc == 3 {
		po += 1 + int(pkt[4])
	}
	if po+3 > tsPacketSize {
		return nil
	}
	ptr := int(pkt[po])
	start := po + 1 + ptr
	if start > tsPacketSize {
		return nil
	}
	s := pkt[start:]
	if len(s) < 12 || s[0] != wantTable {
		return nil
	}
	slen := (int(s[1]&0x0f) << 8) | int(s[2])
	if 3+slen < 12 || 3+slen > len(s) {
		return nil
	}
	return s[:3+slen]
}

// extractPMTSection: 同 tsSection 但专门取 PMT(表 id 0x02)
func extractPMTSection(pkt []byte) []byte { return tsSection(pkt, 0x02) }

// rewritePMTPkt: 用重写后的 PMT section 替换一个 PMT 包: 保留 TS 头,
// 载荷=指针字段+新 section+0xFF 填充。
func rewritePMTPkt(pkt []byte, newSection []byte) []byte {
	if len(newSection) > tsPacketSize-5 {
		return pkt
	}
	p := make([]byte, tsPacketSize)
	copy(p, pkt[:4])                                // TS 头
	p[1] |= 0x40                                    // payload_unit_start_indicator
	p[3] = (pkt[3] & 0xC0) | 0x10 | (pkt[3] & 0x0F) // 纯载荷(afc=1), 保留 CC
	p[4] = 0                                        // pointer_field
	copy(p[5:], newSection)
	for j := 5 + len(newSection); j < tsPacketSize; j++ {
		p[j] = 0xFF // 填充
	}
	return p
}

// mpegCRC32: MPEG-2 PSI section 的 CRC-32 (poly 0x04C11DB7, 非反射)
func mpegCRC32(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc & 0xFFFFFFFF
}
