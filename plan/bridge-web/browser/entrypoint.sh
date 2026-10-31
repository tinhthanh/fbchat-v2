#!/bin/sh
# fbweb-browser: Chromium (extension fb-bridge-web nạp sẵn) mở Hộp thư Business Suite.
#   FBWEB_MODE=run (mặc định): Xvfb + openbox + Chromium, không có VNC.
#   FBWEB_MODE=login: thêm x11vnc + noVNC (:6090) để đăng nhập hoặc xem.
# Hai chế độ dùng chung profile trong volume /data: đăng nhập một lần ở "login", rồi
# chạy "run". Chromium bị tắt hay crash thì mở lại.
# Không dùng --headless=new: đo 31/10/2026, GPU process tự dựng khung liên tục (~45%
# CPU, ~500 MiB), --enable-begin-frame-control không có tác dụng ở chế độ đó.
set -eu

: "${FBWEB_TOKEN:?thiếu FBWEB_TOKEN}"
: "${FBWEB_ACCOUNT:?thiếu FBWEB_ACCOUNT}"
: "${FBWEB_PAGE_ID:?thiếu FBWEB_PAGE_ID}"
FBWEB_URL="${FBWEB_URL:-http://host.docker.internal:7008}"
MODE="${FBWEB_MODE:-run}"
SCREEN="${SCREEN:-1280x720}"
PROFILE=/data/profile
EXT=/tmp/fbweb-ext

# Extension chạy từ bản sao trong tmpfs: image read-only, config chứa token chỉ nằm trong RAM.
rm -rf "$EXT" && cp -r /opt/fbweb-ext "$EXT"
umask 077
printf '{"url":"%s","token":"%s","account":"%s"}\n' "$FBWEB_URL" "$FBWEB_TOKEN" "$FBWEB_ACCOUNT" > "$EXT/config.json"
umask 022
# Trang không tự tải video (Chromium Alpine crash renderer khi giải mã H.264, vd. GIF
# Facebook đổi thành mp4; đo 31/10/2026) và, ở chế độ run, cũng không tải ảnh. File
# đính kèm vẫn đến: service worker của extension tự tải, luật chỉ áp cho yêu cầu của trang.
FRONT_TAB=""
if [ "$MODE" != login ]; then
    # Tab Business Suite nằm sau một tab trống: Chromium coi renderer là tab nền thật
    # (nén chuỗi mã nguồn, xả cache, trả RAM), như tab nền của Chrome thường.
    FRONT_TAB="about:blank"
    cp "$EXT/quiet-run.js" "$EXT/quiet.js"
    printf '%s\n' '[{"id":1,"priority":1,"action":{"type":"block"},"condition":{"initiatorDomains":["business.facebook.com"],"resourceTypes":["media","image"]}}]' > "$EXT/rules.json"
fi

mkdir -p "$PROFILE"
# Container trước bị dừng đột ngột để lại khoá profile: Chromium sẽ không mở.
rm -f "$PROFILE/SingletonLock" "$PROFILE/SingletonSocket" "$PROFILE/SingletonCookie"

URL="https://business.facebook.com/latest/inbox/all/?asset_id=${FBWEB_PAGE_ID}"
# FBWEB_DEBUG=1: in console của trang ra log container (dòng [BS-INBOX]).
LOG=/dev/null
DEBUG_FLAGS=""
if [ "${FBWEB_DEBUG:-0}" = 1 ]; then
    LOG=/dev/stdout
    DEBUG_FLAGS="--enable-logging=stderr --v=0 --remote-debugging-port=9222 --remote-debugging-address=127.0.0.1"
fi

case "$MODE" in
login | run)
    Xvfb :99 -screen 0 "${SCREEN}x24" -nolisten tcp &
    sleep 1
    # Không có window manager thì click không chuyển focus bàn phím giữa các cửa sổ.
    openbox &
    if [ "$MODE" = login ]; then
        if [ -n "${VNC_PASSWORD:-}" ]; then
            x11vnc -storepasswd "$VNC_PASSWORD" /tmp/vncpass >/dev/null 2>&1
            set -- -rfbauth /tmp/vncpass
        else
            set -- -nopw
        fi
        x11vnc -display :99 -forever -shared -localhost -rfbport 5900 -quiet "$@" &
        websockify --web /usr/share/novnc 6090 localhost:5900 >/dev/null 2>&1 &
        echo "fbweb-browser: login mode, noVNC http://localhost:6090/vnc.html?autoconnect=1&resize=scale"
    else
        echo "fbweb-browser: run mode, không có noVNC (đăng nhập lại: FBWEB_MODE=login docker compose up -d)"
    fi
    set -- --start-maximized
    ;;
headless)
    # Thử nghiệm: không Xvfb/openbox. User agent đặt lại như bản có giao diện (headless tự
    # khai "HeadlessChrome"); quiet-run.js tắt animation nên GPU process không dựng khung liên tục.
    major=$(chromium-browser --version | grep -oE '[0-9]+' | head -1)
    set -- --headless=new --window-size=1280,720 \
        --user-agent="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${major}.0.0.0 Safari/537.36"
    echo "fbweb-browser: headless mode"
    ;;
*)
    echo "FBWEB_MODE phải là run, headless hoặc login" >&2
    exit 1
    ;;
esac
echo "fbweb-browser: ingest ${FBWEB_URL}, page ${FBWEB_PAGE_ID}"

# Watchdog: tab crash ("Aw, Snap") thì không còn renderer nào ngoài renderer của
# extension; mất 2 phút liền thì khởi động lại Chromium. Cứ RESTART_HOURS giờ khởi
# động lại một lần để xả bộ nhớ Business Suite tăng dần. Tin đến lúc khởi động lại
# vẫn tới: Facebook gửi lại dạng upsertMessage còn mới khi trang đồng bộ.
RESTART_HOURS="${RESTART_HOURS:-6}"
watchdog() {
    misses=0
    while sleep 60; do
        pid=$(cat /tmp/chromium.pid 2>/dev/null) || continue
        page=0
        for r in $(pgrep -f -- "--type=renderer"); do
            grep -q -- "--extension-process" "/proc/$r/cmdline" 2>/dev/null || page=1
        done
        if [ "$page" = 1 ]; then misses=0; else misses=$((misses + 1)); fi
        age=$(( $(date +%s) - $(stat -c %Y /tmp/chromium.pid) ))
        if [ "$misses" -ge 2 ] || [ "$age" -ge $((RESTART_HOURS * 3600)) ]; then
            echo "fbweb-browser: watchdog restart ($([ "$misses" -ge 2 ] && echo "tab crashed" || echo "periodic"))"
            kill "$pid" 2>/dev/null
            misses=0
        fi
    done
}
watchdog &

while :; do
    chromium-browser \
        --no-sandbox --test-type \
        --disable-dev-shm-usage --disable-gpu \
        --no-zygote --disable-crash-reporter --disable-breakpad \
        --no-first-run --no-default-browser-check --password-store=basic \
        --user-data-dir="$PROFILE" \
        --load-extension="$EXT" --disable-extensions-except="$EXT" \
        --disable-features=DisableLoadExtensionCommandLineSwitch,Translate,MediaRouter,OptimizationHints,AutofillServerCommunication,AudioServiceOutOfProcess,BackForwardCache,SpareRendererForSitePerProcess,MemoryCacheStrongReference \
        --enable-features=NetworkServiceInProcess \
        --disable-component-update --disable-sync --disable-default-apps --mute-audio \
        --renderer-process-limit=2 \
        --enable-low-end-device-mode --js-flags="--max-semi-space-size=2 --lite-mode --optimize-for-size" \
        --lang=vi-VN $DEBUG_FLAGS "$@" \
        $FRONT_TAB "$URL" >"$LOG" 2>&1 &
    echo $! > /tmp/chromium.pid
    wait $! || true
    echo "fbweb-browser: Chromium exited, restarting in 3s"
    sleep 3
done
