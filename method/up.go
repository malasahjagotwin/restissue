package main

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	uaDesktop = ""
	uaMobile  = ""

	secChUaVal      = `"Chromium";v="136", "Google Chrome";v="136", "Not-A.Brand";v="99"`
	platformDesktop = `"Windows"`
	platformMobile  = `"Android"`
)

type statsType struct {
	total   int64
	success int64
	failed  int64
}

var stats statsType

// mejiChars adalah charset untuk value random query param
const mejiChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// mejiSuffixes adalah suffix yang bisa ditempel setelah "meji" sebagai key
var mejiSuffixes = []string{
	"", "App", "Load", "Ts", "Req", "Id", "Cache", "Rand", "Hit", "Src",
	"Tk", "Ver", "Sid", "Uid", "Tag", "Ctx", "Ref", "Env", "Run", "Seq",
}

// randStr generate random string dengan panjang n dari mejiChars
func randStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = mejiChars[rand.Intn(len(mejiChars))]
	}
	return string(b)
}

// randomMejiQuery build query string random berawalan "meji"
// contoh output: "meji=a3x9", "mejiLoad=yz12&mejiTs=891k"
func randomMejiQuery() string {
	// jumlah params: 1 s/d 3
	nParams := rand.Intn(3) + 1
	pairs := make([]string, 0, nParams)
	used := make(map[string]bool)

	for i := 0; i < nParams; i++ {
		var key string
		// cegah duplikat key
		for {
			suffix := mejiSuffixes[rand.Intn(len(mejiSuffixes))]
			key = "meji" + suffix
			if !used[key] {
				used[key] = true
				break
			}
		}
		val := randStr(rand.Intn(8) + 4) // panjang value 4-11 char
		pairs = append(pairs, key+"="+val)
	}
	return strings.Join(pairs, "&")
}

// appendMejiQuery tambah random meji query ke URL,
// handle kalau URL udah ada '?' atau belum.
// Kalau URL tanpa path (misal https://example.com),
// otomatis ditambah '/' supaya jadi https://example.com/?meji=...
func appendMejiQuery(rawURL string) string {
	q := randomMejiQuery()
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + q
	}
	// Cek apakah ada path setelah scheme://host
	// https://example.com → tidak ada path → tambah /
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd >= 0 {
		afterScheme := rawURL[schemeEnd+3:]
		if !strings.Contains(afterScheme, "/") {
			return rawURL + "/?" + q
		}
	}
	return rawURL + "?" + q
}

func buildClient() *fasthttp.Client {
	return &fasthttp.Client{
		TLSConfig: &tls.Config{
			InsecureSkipVerify:       true,
			CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP256},
			PreferServerCipherSuites: true,
			MinVersion:               tls.VersionTLS12,
			MaxVersion:               tls.VersionTLS13,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		},
		MaxConnsPerHost:     10000,
		MaxIdleConnDuration: 30 * time.Second,
		ReadTimeout:         10 * time.Second,
		WriteTimeout:        10 * time.Second,
	}
}

func setHeaders(req *fasthttp.Request, isMobile bool) {
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
	req.Header.Set("Cache-Control", "max-age=0")
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

func makeRequest(client *fasthttp.Client, target string, isMobile bool) bool {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(appendMejiQuery(target))
	req.Header.SetMethod(fasthttp.MethodGet)
	setHeaders(req, isMobile)

	err := client.DoTimeout(req, resp, 10*time.Second)
	if err != nil {
		return false
	}

	return resp.StatusCode() >= 200 && resp.StatusCode() < 500
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: hget <url> <duration_seconds> <rate_per_second>")
		fmt.Println("Example: hget https://example.com 10 100")
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

	client := buildClient()

	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	deadline := time.After(time.Duration(duration) * time.Second)
	var wg sync.WaitGroup

	fmt.Printf("Starting fasthttp requests to %s\n", target)
	fmt.Printf("Duration: %ds | Rate: %d req/s\n\n", duration, rate)

	start := time.Now()

loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			wg.Add(1)
			atomic.AddInt64(&stats.total, 1)
			go func() {
				defer wg.Done()

				isMobile := rand.Intn(100) < 40

				if makeRequest(client, target, isMobile) {
					atomic.AddInt64(&stats.success, 1)
				} else {
					atomic.AddInt64(&stats.failed, 1)
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
	fmt.Printf("Total Req   : %d\n", stats.total)
	fmt.Printf("Success     : %d\n", stats.success)
	fmt.Printf("Failed      : %d\n", stats.failed)
	fmt.Printf("Avg Rate    : %.2f req/s\n", float64(stats.total)/elapsed)
	fmt.Println("=============================")
}
