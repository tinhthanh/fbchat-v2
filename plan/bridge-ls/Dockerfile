# Build context: thư mục này; mautrix-meta (messagix/dgw) đến từ context phụ
# "meta" = bridge-e2ee/meta (docker-compose.yml), đúng đường replace của go.mod.
FROM golang:1.26.10-alpine AS build
WORKDIR /src/plan/bridge-ls
COPY --from=meta . /src/bridge-e2ee/meta
COPY go.mod go.sum ./
COPY *.go ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fbweb-ls . \
    && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/fbweb-ls /fbweb-ls
# uid 1000 như fb-login-ui: chế độ agent đọc cookie fb-login-ui ghi (quyền 600).
COPY --from=build --chown=1000:1000 /out/data /data
USER 1000:1000
ENTRYPOINT ["/fbweb-ls"]
