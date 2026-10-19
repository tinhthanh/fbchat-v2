# Trình duyệt đăng nhập tạm thời: Chromium + Xvfb + x11vnc + noVNC trên Alpine.
# Container tự thoát (exit 0) ngay khi lấy được cookie c_user/xs.
FROM alpine:3.22

RUN apk add --no-cache \
        chromium \
        chromium-swiftshader \
        font-dejavu \
        novnc \
        openbox \
        py3-cryptography \
        python3 \
        tini \
        websockify \
        x11vnc \
        xvfb \
    && addgroup -g 1000 fbchat \
    && adduser -D -u 1000 -G fbchat fbchat \
    && mkdir -p /data /etc/chromium/policies/managed \
    && chown fbchat:fbchat /data \
    # Chặn DevTools (cửa sổ mở nhầm sẽ che trang đăng nhập) và không lưu mật khẩu.
    && echo '{"DeveloperToolsAvailability": 2, "PasswordManagerEnabled": false}' \
        > /etc/chromium/policies/managed/fbchat.json

COPY --chmod=0755 extract_cookie.py /usr/local/bin/extract_cookie.py

USER fbchat
WORKDIR /home/fbchat
ENV PYTHONUNBUFFERED=1 \
    DISPLAY=:99

EXPOSE 6080
ENTRYPOINT ["/sbin/tini", "--", "python3", "/usr/local/bin/extract_cookie.py"]
