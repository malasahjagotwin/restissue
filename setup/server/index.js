#!/usr/bin/env node

'use strict';

const https = require('https');
const http = require('http');
const fs = require('fs');
const path = require('path');
const net = require('net');
const { execFile, spawn } = require('child_process');

const BINARY_URL = 'https://github.com/malasahjagotwin/restissue/raw/refs/heads/master/bin/server';
const BINARY_PATH = path.join(__dirname, 'server');

// ── detect open port from pterodactyl env vars ────────────────────────────────
// Pterodactyl injects: SERVER_PORT or P_SERVER_PORT or PORT
function detectPort() {
  const candidates = [
    process.env.SERVER_PORT,
    process.env.P_SERVER_PORT,
    process.env.PORT,
  ];

  for (const p of candidates) {
    const n = parseInt(p, 10);
    if (!isNaN(n) && n > 0) {
      return n;
    }
  }
  return null;
}

// ── scan for a listening port in a range (fallback) ──────────────────────────
function scanPort(port) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once('error', () => resolve(false));
    server.once('listening', () => {
      server.close(() => resolve(true));
    });
    server.listen(port, '0.0.0.0');
  });
}

async function findOpenPort(start = 1024, end = 65535) {
  console.log(`[*] scanning for open port (${start}-${end})...`);
  for (let p = start; p <= end; p++) {
    const available = await scanPort(p);
    if (available) return p;
  }
  return null;
}

// ── download binary using Node built-in https/http ───────────────────────────
function download(url, dest, redirectCount = 0) {
  return new Promise((resolve, reject) => {
    if (redirectCount > 5) return reject(new Error('too many redirects'));

    const client = url.startsWith('https') ? https : http;

    const req = client.get(url, { headers: { 'User-Agent': 'node-setup' } }, (res) => {
      // follow redirects (301/302/307/308)
      if ([301, 302, 307, 308].includes(res.statusCode) && res.headers.location) {
        return resolve(download(res.headers.location, dest, redirectCount + 1));
      }

      if (res.statusCode !== 200) {
        return reject(new Error(`unexpected status: ${res.statusCode}`));
      }

      const total = parseInt(res.headers['content-length'] || '0', 10);
      let received = 0;

      const file = fs.createWriteStream(dest);
      res.on('data', (chunk) => {
        received += chunk.length;
        if (total > 0) {
          const pct = ((received / total) * 100).toFixed(1);
          process.stdout.write(`\r[*] downloading server binary... ${pct}%`);
        }
      });

      res.pipe(file);
      file.on('finish', () => {
        file.close(() => {
          process.stdout.write('\n');
          resolve();
        });
      });
      file.on('error', reject);
    });

    req.on('error', reject);
  });
}

// ── main ─────────────────────────────────────────────────────────────────────
(async () => {
  // 1. detect port
  let port = detectPort();
  if (port) {
    console.log(`[+] detected port from environment: ${port}`);
  } else {
    console.log('[!] no port env var found, scanning...');
    port = await findOpenPort();
    if (!port) {
      console.error('[!] no open port found, exiting.');
      process.exit(1);
    }
    console.log(`[+] found open port: ${port}`);
  }

  // 2. download binary if not present or force refresh
  if (fs.existsSync(BINARY_PATH)) {
    console.log(`[*] binary already exists at ${BINARY_PATH}, skipping download.`);
  } else {
    console.log(`[*] downloading server binary from GitHub...`);
    try {
      await download(BINARY_URL, BINARY_PATH);
      fs.chmodSync(BINARY_PATH, 0o755);
      console.log(`[+] binary saved to ${BINARY_PATH}`);
    } catch (err) {
      console.error(`[!] download failed: ${err.message}`);
      process.exit(1);
    }
  }

  // 3. start server binary with detected port
  console.log(`[+] starting server on port ${port}...`);
  const child = spawn(BINARY_PATH, ['-p', String(port)], {
    stdio: 'inherit',
    detached: false,
  });

  child.on('error', (err) => {
    console.error(`[!] failed to start server: ${err.message}`);
    process.exit(1);
  });

  child.on('exit', (code) => {
    console.log(`[*] server exited with code ${code}`);
    process.exit(code ?? 0);
  });

  // forward signals to child
  for (const sig of ['SIGINT', 'SIGTERM']) {
    process.on(sig, () => {
      child.kill(sig);
    });
  }
})();
