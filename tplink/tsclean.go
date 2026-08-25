package tplink

import "bytes"

// tsCleaner: 把 NVR 的私有 TS 清洗成标准单视频流。
//
// NVR preview 流的 PMT 里声明了两个 ES: 0x24=HEVC 视频 + 0x92=私有流(内容为 0xd5 填充)。
// ffmpeg/VLC 不认识 0x92，默认 probesize=5MB 时会一直读到 5MB 试图识别它，
// 而流是 ~250KB/s 的实时节奏 → 客户端首帧要等 ~20 秒。清洗后只剩一路 HEVC。
type tsCleaner struct {
	buf      []byte // <188 字节的残留
	pmtPID   int    // 从 PAT 学; -1=未知
	videoPID int    // 从 PMT 学; -1=未知
	cleanPMT []byte // 重写后的干净 PMT section(表格字节, 不含指针字段)
}

func newTSCleaner() *tsCleaner {
	return &tsCleaner{pmtPID: -1, videoPID: -1}
}

// feed: 输入任意长度的 TS 字节流(可跨 part 边界), 返回清洗后的标准 TS 字节。
func (c *tsCleaner) feed(in []byte) []byte {
	c.buf = append(c.buf, in...)
	var out []byte
	n := len(c.buf)
	start := 0
	for start+188 <= n {
		if c.buf[start] != 0x47 {
			// 丢开对齐前, 找下一个 0x47 作为包边界(最多往后扫 188*2 字节)
			lim := start + 188*2
			if lim > n {
				lim = n
			}
			j := bytes.IndexByte(c.buf[start+1:lim], 0x47)
			if j < 0 {
				break
			}
			start += 1 + j
			if start+188 > n {
				break
			}
		}
		pkt := c.buf[start : start+188]
		pid := (int(pkt[1]&0x1f) << 8) | int(pkt[2])
		switch {
		case pid == 0: // PAT 学习 PMT PID 并透传
			c.learnPAT(pkt)
			out = append(out, pkt...)
		case c.videoPID >= 0 && pid == c.videoPID:
			out = append(out, pkt...)
		case c.pmtPID >= 0 && pid == c.pmtPID:
			if c.cleanPMT == nil {
				if sec := extractPMTSection(pkt); sec != nil {
					c.cleanPMT = c.rebuildPMT(sec)
				}
			}
			if c.cleanPMT != nil {
				out = append(out, rewritePMTPkt(pkt, c.cleanPMT)...)
			} else {
				out = append(out, pkt...) // 还没学到就原样转发
			}
		default: // 私有流 0x92 / null / SDT 等一律丢弃
		}
		start += 188
	}
	c.buf = append(c.buf[:0], c.buf[start:]...)
	return out
}

// learnPAT: 解析 PAT(表 id 0x00) 找到 PMT PID
func (c *tsCleaner) learnPAT(pkt []byte) {
	s := tsSection(pkt, 0x00)
	if s == nil || len(s) < 12 {
		return
	}
	slen := (int(s[1]&0x0f) << 8) | int(s[2])
	end := 3 + slen - 4
	if end > len(s) {
		end = len(s)
	}
	pos := 8
	for pos+4 <= end {
		pn := (int(s[pos]&0x1f) << 8) | int(s[pos+1])
		if pn != 0 {
			c.pmtPID = (int(s[pos+2]&0x1f) << 8) | int(s[pos+3])
		}
		pos += 4
	}
}

// tsSection: 从 TS 包提取完整 PSI section(含 CRC), 校验 table id; 跨包/不完整的返回 nil
func tsSection(pkt []byte, wantTable byte) []byte {
	afc := pkt[3] & 0x03
	if afc != 1 && afc != 3 {
		return nil
	}
	po := 4
	if afc == 3 {
		po += 1 + int(pkt[4])
	}
	if po+3 > 188 {
		return nil
	}
	ptr := int(pkt[po])
	s := pkt[po+1+ptr:]
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

// rebuildPMT: 重写 PMT section —— 只保留视频 ES(0x1b H.264 / 0x24 HEVC),
// 更新 section_length 并重算 MPEG CRC32。同时把视频 PID 回写到 c.videoPID。
func (c *tsCleaner) rebuildPMT(sec []byte) []byte {
	if len(sec) < 16 {
		return nil
	}
	body := sec[:len(sec)-4] // 去掉 CRC
	pos := 12 + int((body[10]&0x0f)<<8|body[11])
	var kept []byte
	newPID := -1
	for pos+5 <= len(body) {
		st := body[pos]
		epid := (int(body[pos+1]&0x1f) << 8) | int(body[pos+2])
		elen := (int(body[pos+3]&0x0f) << 8) | int(body[pos+4])
		if pos+5+elen > len(body) {
			break
		}
		if st == 0x1b || st == 0x24 {
			kept = append(kept, body[pos:pos+5+elen]...)
			newPID = epid
		}
		pos += 5 + elen
	}
	if newPID < 0 {
		return nil
	}
	c.videoPID = newPID
	nb := append([]byte{}, body[:12]...)
	nb = append(nb, kept...)
	newLen := len(nb) - 3 + 4 // section_length 字段的语义: 其后到 CRC 结束
	nb[1] = 0x30 | byte((newLen>>8)&0x0f)
	nb[2] = byte(newLen & 0xff)
	crc := mpegCRC32(nb)
	nb = append(nb, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return nb
}

// rewritePMTPkt: 用干净 PMT section 重写一个 PMT 包: 保留 TS 头, 载荷=指针字段+新 section+0xFF 填充
func rewritePMTPkt(pkt []byte, newSection []byte) []byte {
	p := make([]byte, 188)
	copy(p, pkt[:4])              // TS 头
	p[1] |= 0x40                  // payload_unit_start_indicator
	p[3] = pkt[3]&0xf0 | 0x01     // 纯载荷, 无 adaptation field
	p[4] = 0                      // pointer_field
	copy(p[5:], newSection)       // 新 PMT section
	for j := 5 + len(newSection); j < 188; j++ {
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