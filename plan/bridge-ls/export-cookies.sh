#!/bin/sh
# Lấy cookie phiên facebook.com từ Chromium của fbweb-browser (qua CDP bên trong
# container) và ghi vào data/cookies.json (quyền 600). Cần fbweb-browser chạy với
# FBWEB_DEBUG=1 (cổng CDP chỉ mở trong container):
#   (cd ../bridge-web/browser && FBWEB_DEBUG=1 docker compose up -d)
# Chỉ in tên cookie, không in giá trị. Không gửi gì lên Facebook.
set -eu
cd "$(dirname "$0")"
CONTAINER="${FBWEB_BROWSER_CONTAINER:-fbweb-browser-browser-1}"
if ! docker exec "$CONTAINER" wget -qO- http://127.0.0.1:9222/json >/dev/null 2>&1; then
    echo "CDP chưa mở trong $CONTAINER. Chạy: (cd ../bridge-web/browser && FBWEB_DEBUG=1 docker compose up -d)" >&2
    exit 1
fi
mkdir -p data && chmod 700 data
umask 077
docker exec -i "$CONTAINER" python3 - > data/cookies.json.tmp <<'PY'
import base64, json, os, socket, struct, urllib.request
from urllib.parse import urlsplit
t = next(t for t in json.load(urllib.request.urlopen("http://127.0.0.1:9222/json"))
         if t["type"] == "page" and "facebook.com" in t["url"])
u = urlsplit(t["webSocketDebuggerUrl"])
s = socket.create_connection((u.hostname, u.port))
s.sendall((f"GET {u.path} HTTP/1.1\r\nHost: {u.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
           f"Sec-WebSocket-Key: {base64.b64encode(os.urandom(16)).decode()}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
buf = b""
while b"\r\n\r\n" not in buf:
    buf += s.recv(4096)
buf = buf.split(b"\r\n\r\n", 1)[1]
def recvn(n):
    global buf
    while len(buf) < n:
        buf += s.recv(1 << 20)
    r, buf = buf[:n], buf[n:]
    return r
p = json.dumps({"id": 1, "method": "Network.getCookies", "params": {"urls": [
    "https://www.facebook.com/", "https://business.facebook.com/", "https://gateway.facebook.com/"]}}).encode()
m = os.urandom(4)
hdr = bytes([0x81, 0x80 | 126]) + struct.pack(">H", len(p))
s.sendall(hdr + m + bytes(b ^ m[i % 4] for i, b in enumerate(p)))
while True:
    data = b""
    while True:
        b0, b1 = recvn(2)
        n = b1 & 0x7F
        n = struct.unpack(">H", recvn(2))[0] if n == 126 else struct.unpack(">Q", recvn(8))[0] if n == 127 else n
        data += recvn(n)
        if b0 & 0x80:
            break
    msg = json.loads(data)
    if msg.get("id") == 1:
        break
cookies = {}
for c in sorted(msg["result"]["cookies"], key=lambda c: c["domain"] != ".facebook.com"):
    if c["domain"].lstrip(".").endswith("facebook.com"):
        cookies.setdefault(c["name"], c["value"])
print(json.dumps(cookies))
PY
if ! python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if d.get("c_user") and d.get("xs") else 1)' data/cookies.json.tmp; then
    rm -f data/cookies.json.tmp
    echo "Không có c_user/xs: fbweb-browser chưa đăng nhập?" >&2
    exit 1
fi
mv data/cookies.json.tmp data/cookies.json
python3 -c 'import json,sys; print("cookies:", ", ".join(sorted(json.load(open(sys.argv[1])))))' data/cookies.json
