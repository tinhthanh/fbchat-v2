#!/bin/sh
# fbweb-browser: Xvfb + Chromium (extension fb-bridge-web nạp sẵn) + noVNC để đăng nhập.
# Chromium bị tắt hay crash thì mở lại; profile nằm trong volume /data nên không phải
# đăng nhập lại.
set -eu

: "${FBWEB_TOKEN:?thiếu FBWEB_TOKEN}"
: "${FBWEB_ACCOUNT:?thiếu FBWEB_ACCOUNT}"
: "${FBWEB_PAGE_ID:?thiếu FBWEB_PAGE_ID}"
FBWEB_URL="${FBWEB_URL:-http://host.docker.internal:7008}"
SCREEN="${SCREEN:-1280x800}"
PROFILE=/data/profile
EXT=/tmp/fbweb-ext

# Extension chạy từ bản sao trong tmpfs: image read-only, config chứa token chỉ nằm trong RAM.
rm -rf "$EXT" && cp -r /opt/fbweb-ext "$EXT"
umask 077
printf '{"url":"%s","token":"%s","account":"%s"}\n' "$FBWEB_URL" "$FBWEB_TOKEN" "$FBWEB_ACCOUNT" > "$EXT/config.json"
umask 022

mkdir -p "$PROFILE"
# Container trước bị dừng đột ngột để lại khoá profile: Chromium sẽ không mở.
rm -f "$PROFILE/SingletonLock" "$PROFILE/SingletonSocket" "$PROFILE/SingletonCookie"

Xvfb :99 -screen 0 "${SCREEN}x24" -nolisten tcp &
sleep 1
# Không có window manager thì click không chuyển focus bàn phím giữa các cửa sổ.
openbox &

if [ -n "${VNC_PASSWORD:-}" ]; then
    x11vnc -storepasswd "$VNC_PASSWORD" /tmp/vncpass >/dev/null 2>&1
    set -- -rfbauth /tmp/vncpass
else
    set -- -nopw
fi
x11vnc -display :99 -forever -shared -localhost -rfbport 5900 -quiet "$@" &
websockify --web /usr/share/novnc 6090 localhost:5900 >/dev/null 2>&1 &

URL="https://business.facebook.com/latest/inbox/all/?asset_id=${FBWEB_PAGE_ID}"
# FBWEB_DEBUG=1: in console của trang và extension ra log container (dòng [BS-INBOX], fb-bridge-web).
LOG=/dev/null
DEBUG_FLAGS=""
if [ "${FBWEB_DEBUG:-0}" = 1 ]; then
    LOG=/dev/stdout
    DEBUG_FLAGS="--enable-logging=stderr --v=0"
fi
echo "fbweb-browser: noVNC http://localhost:6090/vnc.html?autoconnect=1&resize=scale, ingest ${FBWEB_URL}, page ${FBWEB_PAGE_ID}"

while :; do
    chromium-browser \
        --no-sandbox --test-type \
        --disable-dev-shm-usage --disable-gpu \
        --no-first-run --no-default-browser-check --password-store=basic \
        --user-data-dir="$PROFILE" \
        --load-extension="$EXT" --disable-extensions-except="$EXT" \
        --disable-features=DisableLoadExtensionCommandLineSwitch,Translate,MediaRouter,OptimizationHints,AutofillServerCommunication \
        --disable-background-timer-throttling --disable-renderer-backgrounding --disable-backgrounding-occluded-windows \
        --disable-component-update --disable-sync --disable-default-apps --mute-audio \
        --renderer-process-limit=2 \
        --lang=vi-VN --start-maximized $DEBUG_FLAGS \
        "$URL" >"$LOG" 2>&1 || true
    echo "fbweb-browser: Chromium exited, restarting in 3s"
    sleep 3
done
