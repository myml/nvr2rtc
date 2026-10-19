package tplink

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildAudioTS: PAT + PMT(视频 0x101 + 私有音频 0x102) + 一个视频包 + 一个
// WAVE/A-law 音频 PES（跨 6 个 TS 包）。PTS 固定，便于逐字节比较。
func buildAudioTS() []byte {
	pat := tsPkt(0, []byte{
		0x00, 0xB0, 0x0D,
		0x00, 0x01,
		0x01, 0x00, 0x00,
		0x00, 0x01, 0xE1, 0x00,
		0x01, 0x02, 0x03, 0x04,
	})
	pmt := tsPkt(0x100, []byte{
		0x02, 0xB0, 0x17,
		0x00, 0x01,
		0x01, 0x00, 0x00,
		0xE1, 0x01, // PCR PID 0x101
		0x00, 0x00,
		0x24, 0xE1, 0x01, 0x00, 0x00, // HEVC 0x101
		0x92, 0xE1, 0x02, 0x00, 0x00, // TP-Link 私有音频 0x102
		0xDE, 0xAD, 0xBE, 0xEF,
	})

	vid := make([]byte, 188)
	vid[0] = 0x47
	vid[1] = 0x41 // pid 0x101 + PUSI
	vid[2] = 0x01
	vid[3] = 0x11
	copy(vid[4:], []byte{0x00, 0x00, 0x00, 0x01, 0xE0})

	pes := makeWavPES()
	audio := muxPESForTest(0x102, pes)

	out := append([]byte{}, pat...)
	out = append(out, pmt...)
	out = append(out, vid...)
	out = append(out, audio...)
	// 一个 null 包，用于验证 clean 模式确实丢弃 null
	null := make([]byte, 188)
	null[0] = 0x47
	null[1] = 0x1F
	null[2] = 0xFF
	null[3] = 0x10
	out = append(out, null...)
	return out
}

// makeWavPES: 构造一个与 NVR 实测一致的私有 PES: WAVE(fmt=6 A-law) + PTS。
func makeWavPES() []byte {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = 0xD5 // A-law 静音
	}
	var w bytes.Buffer
	w.WriteString("RIFF")
	_ = binary.Write(&w, binary.LittleEndian, uint32(36+len(data)))
	w.WriteString("WAVE")
	w.WriteString("fmt ")
	_ = binary.Write(&w, binary.LittleEndian, uint32(16))
	_ = binary.Write(&w, binary.LittleEndian, uint16(6))    // WAVE_FORMAT_ALAW
	_ = binary.Write(&w, binary.LittleEndian, uint16(1))    // mono
	_ = binary.Write(&w, binary.LittleEndian, uint32(8000)) // sample rate
	_ = binary.Write(&w, binary.LittleEndian, uint32(8000)) // byte rate
	_ = binary.Write(&w, binary.LittleEndian, uint16(1))    // block align
	_ = binary.Write(&w, binary.LittleEndian, uint16(8))    // bits
	w.WriteString("data")
	_ = binary.Write(&w, binary.LittleEndian, uint32(len(data)))
	w.Write(data)

	payload := w.Bytes()
	length := 8 + len(payload)
	pes := []byte{0x00, 0x00, 0x01, 0xC0, byte(length >> 8), byte(length), 0x80, 0x80, 0x05, 0x21, 0x00, 0x01, 0x00, 0x01}
	pes = append(pes, payload...)
	return pes
}

// muxPESForTest: 独立的 TS 打包实现（不复用被测代码）。
func muxPESForTest(pid int, pes []byte) []byte {
	var out []byte
	first := true
	cc := 0
	for len(pes) > 0 {
		pkt := make([]byte, 188)
		pkt[0] = 0x47
		pkt[1] = byte((pid >> 8) & 0x1F)
		if first {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pid)
		if len(pes) >= 184 {
			pkt[3] = 0x10 | byte(cc&0x0F)
			copy(pkt[4:], pes[:184])
			pes = pes[184:]
		} else {
			afl := 183 - len(pes)
			pkt[3] = 0x30 | byte(cc&0x0F)
			pkt[4] = byte(afl)
			if afl > 0 {
				pkt[5] = 0
				for i := 6; i < 5+afl; i++ {
					pkt[i] = 0xFF
				}
			}
			copy(pkt[5+afl:], pes)
			pes = nil
		}
		cc = (cc + 1) & 0x0F
		first = false
		out = append(out, pkt...)
	}
	return out
}

func parseES(t *testing.T, pmt []byte) map[int]int {
	t.Helper()
	es := map[int]int{}
	body := pmt[:len(pmt)-4]
	pos := 12 + (int(body[10]&0x0F)<<8 | int(body[11]))
	for pos+5 <= len(body) {
		st := int(body[pos])
		pid := (int(body[pos+1]&0x1F) << 8) | int(body[pos+2])
		elen := (int(body[pos+3]&0x0F) << 8) | int(body[pos+4])
		es[st] = pid
		pos += 5 + elen
	}
	return es
}

func TestTSRemuxAudio(t *testing.T) {
	in := buildAudioTS()
	out := newTSRemuxer(false).feed(in)
	if len(out) == 0 || len(out)%188 != 0 {
		t.Fatalf("output not TS: %d bytes", len(out))
	}

	var pmt []byte
	var audioPayload, videoPkt []byte
	for off := 0; off+188 <= len(out); off += 188 {
		pkt := out[off : off+188]
		pid := (int(pkt[1]&0x1F) << 8) | int(pkt[2])
		switch pid {
		case 0x100:
			if s := extractPMTSection(pkt); s != nil {
				pmt = s
			}
		case 0x101:
			videoPkt = pkt
		case 0x102:
			if p, _ := tsPayload(pkt); p != nil {
				audioPayload = append(audioPayload, p...)
			}
		}
	}
	if pmt == nil {
		t.Fatal("no PMT in output")
	}
	es := parseES(t, pmt)
	if es[streamTypePCMA] != 0x102 {
		t.Fatalf("PMT audio ES = %v, want 0x90 @0x102; PMT=%x", es, pmt)
	}
	if _, ok := es[streamTypePrivateAudio]; ok {
		t.Fatalf("private 0x92 must be gone; PMT=%x", pmt)
	}
	if es[streamTypeHEVC] != 0x101 {
		t.Fatalf("video ES lost; PMT=%x", pmt)
	}
	if videoPkt == nil {
		t.Fatal("video packet lost")
	}
	const videoOff = 188 * 2 // PAT + PMT
	if !bytes.Equal(videoPkt, in[videoOff:videoOff+188]) {
		t.Fatal("video packet was modified")
	}
	if len(audioPayload) < 14+1024 {
		t.Fatalf("audio payload too short: %d", len(audioPayload))
	}
	if !bytes.Equal(audioPayload[:4], []byte{0x00, 0x00, 0x01, 0xC0}) {
		t.Fatalf("audio PES start code = %x", audioPayload[:4])
	}
	plen := int(audioPayload[4])<<8 | int(audioPayload[5])
	if plen != 8+1024 {
		t.Fatalf("audio PES length = %d, want %d", plen, 8+1024)
	}
	data := audioPayload[9+5 : 6+plen]
	if len(data) != 1024 {
		t.Fatalf("A-law data len = %d", len(data))
	}
	for i, b := range data {
		if b != 0xD5 {
			t.Fatalf("A-law byte %d = %02x, want d5", i, b)
		}
	}
}

func TestTSRemuxChunkBoundaries(t *testing.T) {
	in := buildAudioTS()
	want := newTSRemuxer(false).feed(in)
	r := newTSRemuxer(false)
	var got []byte
	for i := 0; i < len(in); i += 7 {
		end := i + 7
		if end > len(in) {
			end = len(in)
		}
		got = append(got, r.feed(in[i:end])...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("chunked output differs: got %d want %d", len(got), len(want))
	}
}

func TestTSRemuxCleanDropsNull(t *testing.T) {
	in := buildAudioTS()
	out := newTSRemuxer(true).feed(in)
	for off := 0; off+188 <= len(out); off += 188 {
		pid := (int(out[off+1]&0x1F) << 8) | int(out[off+2])
		if pid == 0x1FFF {
			t.Fatalf("clean output still has null PID at %d", off)
		}
	}
	// clean 模式同样要转换音频
	var pmt []byte
	for off := 0; off+188 <= len(out); off += 188 {
		pkt := out[off : off+188]
		if pid := (int(pkt[1]&0x1F) << 8) | int(pkt[2]); pid == 0x100 {
			pmt = extractPMTSection(pkt)
		}
	}
	if pmt == nil || parseES(t, pmt)[streamTypePCMA] != 0x102 {
		t.Fatalf("clean mode did not advertise PCMA: %x", pmt)
	}
}
