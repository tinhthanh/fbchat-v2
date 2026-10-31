# fbweb-browser: Chromium trên Alpine, mở Hộp thư Business Suite với extension
# fb-bridge-web nạp sẵn; noVNC (cổng 6090) chỉ để đăng nhập và xem.
# Build từ plan/bridge-web: docker compose -f browser/docker-compose.yml build
FROM alpine:3.22

RUN apk add --no-cache \
        chromium \
        font-dejavu \
        novnc \
        openbox \
        tini \
        websockify \
        x11vnc \
        xvfb \
    && addgroup -g 1000 fbweb \
    && adduser -D -u 1000 -G fbweb fbweb \
    && mkdir -p /data /etc/chromium/policies/managed \
    && chown fbweb:fbweb /data \
    # Không lưu mật khẩu, không gợi ý đăng nhập Chrome.
    && echo '{"PasswordManagerEnabled": false, "BrowserSignin": 0, "SyncDisabled": true}' \
        > /etc/chromium/policies/managed/fbweb.json

COPY browser/extension/ /opt/fbweb-ext/
# Script nghe socket dùng chung với bản Tampermonkey (một nguồn duy nhất).
COPY bs-inbox-listener.js /opt/fbweb-ext/bs-inbox-listener.js
COPY --chmod=0755 browser/entrypoint.sh /usr/local/bin/fbweb-browser

USER fbweb
WORKDIR /home/fbweb
# Root filesystem read-only: cache fontconfig/openbox nằm trong tmpfs /tmp.
ENV DISPLAY=:99 \
    XDG_CACHE_HOME=/tmp/.cache \
    XDG_CONFIG_HOME=/tmp/.config
EXPOSE 6090
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/fbweb-browser"]
