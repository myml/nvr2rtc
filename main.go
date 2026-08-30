// nvr2rtc: 把 TP-Link NVR 的实时预览流以标准 HTTP MPEG-TS 暴露出来，
// 供 go2rtc / ffmpeg / VLC / ffprobe 直接拉取。
//
// 只需提供 NVR 管理员账号密码：
//
//	nvr2rtc --user admin --pass <密码>
//
// 默认连接 192.168.0.49:8000，监听 0.0.0.0:8081，允许全部 8 路通道。
//
// 端点: http://<host>:8081/ch/<N>    (N = 通道号, 1 起, 与 TP-Link 客户端一致;
// NVR 协议内部通道为 N-1, 见 tplink 包)
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"nvr2rtc/tplink"
)

func main() {
	var (
		nvr    = flag.String("nvr", envOr("NVR2RTC_NVR", "192.168.0.49:8000"), "NVR 地址 host[:port]，如 192.168.0.49 或 192.168.0.49:9000")
		user   = flag.String("user", envOr("NVR2RTC_USER", "admin"), "NVR 管理员用户名")
		pass   = flag.String("pass", envOr("NVR2RTC_PASS", ""), "NVR 管理员密码")
		httpAd = flag.String("http", envOr("NVR2RTC_HTTP", "0.0.0.0:8081"), "HTTP 监听地址")
		allow  = flag.String("ch", "", "通道白名单(逗号分隔, 1 起, 如 1,3,4,6); 默认不限制(任意 1..N 通道, N 取决于 NVR 路数)")
		clean  = flag.Bool("clean", false, "清洗 TS：剔除 TP-Link 私有流(0x92)，只保留视频流；默认透传原始流")
	)
	flag.Parse()

	if *pass == "" {
		fmt.Fprintln(os.Stderr, "错误: 需要 NVR 密码。用法: nvr2rtc --pass <密码> [--user admin] [--nvr 192.168.0.49] [--http 0.0.0.0:8081]")
		flag.Usage()
		os.Exit(1)
	}

	addr := *nvr
	if !strings.Contains(addr, ":") {
		addr += ":8000" // 默认流端口
	}

	// 通道白名单: 空 = 不限制; 指定 = 仅放行列出的通道(其余 403)。
	// 通道号 1 起(对应 TP-Link 客户端的第 1/N 路)。
	var allowed map[int]bool
	if *allow != "" {
		allowed = map[int]bool{}
		for _, p := range strings.Split(*allow, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "无效通道号: %q (应为正整数, 1 起)\n", p)
				os.Exit(1)
			}
			allowed[n] = true
		}
	}

	client := tplink.New(addr, *user, *pass)

	mux := http.NewServeMux()
	mux.HandleFunc("/ch/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		chS := strings.TrimPrefix(r.URL.Path, "/ch/")
		ch, err := strconv.Atoi(chS)
		if err != nil || ch < 1 {
			http.Error(w, "bad channel: /ch/<正整数, 1 起>", 400)
			return
		}
		if allowed != nil && !allowed[ch] {
			http.Error(w, fmt.Sprintf("channel %d not allowed", ch), 403)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 立即发出响应头; 不等第一个 body 字节(无效通道/等待重连时客户端也能立即看到 200)
		}
		// 用户面通道 1 起; NVR 协议通道 = ch-1
		stream := client.Stream(ch-1, *clean)
		clientAddr := r.RemoteAddr
		log.Printf("[/ch/%d] 客户端 %s 连入\n", ch, clientAddr)
		defer log.Printf("[/ch/%d] 客户端 %s 断开\n", ch, clientAddr)
		defer stream.Close()
		buf := make([]byte, 1<<16)
		done := make(chan struct{})
		go func() {
			defer close(done)
			copyTo(w, stream, buf)
		}()
		select {
		case <-done: // copyTo 因写失败/EOF 自行结束
		case <-r.Context().Done():
			// 客户端断开: 无数据流(空通道)时 copyTo 会一直阻塞在读上、
			// 永远不会因写失败发现断开 —— 显式取消订阅并等拷贝协程退出,
			// 避免空通道的 NVR 会话被永久占住(订阅泄漏)
			stream.Close()
			<-done
		}
		return
	})

	chList := "全部"
	if allowed != nil {
		chList = fmt.Sprintf("%v", keys(allowed))
	}
	log.Printf("nvr2rtc: NVR=%s user=%s 通道=%s → http://%s/ch/<N>\n", addr, *user, chList, *httpAd)
	if err := http.ListenAndServe(*httpAd, mux); err != nil {
		log.Fatal("serve exit:", err)
	}
}

func copyTo(w http.ResponseWriter, src interface{ Read([]byte) (int, error) }, buf []byte) (int64, error) {
	var total int64
	for {
		n, e := src.Read(buf)
		if n > 0 {
			if _, we := w.Write(buf[:n]); we != nil {
				return total, we
			}
			total += int64(n)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if e != nil {
			return total, e
		}
	}
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
