#!/bin/bash
set -e

BASE="https://github.com/malasahjagotwin/restissue/raw/refs/heads/master"
PROXY="https://raw.githubusercontent.com/malasahjagotwin/restissue/refs/heads/master"

echo "[*] downloading bot..."
wget -q "$BASE/bin/bot" -O bot && chmod +x bot

echo "[*] downloading up (tls-raw)..."
wget -q "$BASE/bin/up" -O up && chmod +x up

echo "[*] downloading tls-fler..."
wget -q "$BASE/bin/tls-fler" -O tls-fler && chmod +x tls-fler

echo "[*] downloading proxy/global.txt..."
mkdir -p proxy
wget -q "$PROXY/proxy/global.txt" -O proxy/global.txt

echo ""
echo "[+] done. files:"
ls -lh bot up tls-fler proxy/global.txt
