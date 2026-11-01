module fbweb-ls

go 1.26.0

require (
	github.com/coder/websocket v1.8.15
	github.com/imroc/req/v3 v3.56.0
	github.com/refraction-networking/utls v1.8.2
	github.com/rs/zerolog v1.35.1
	go.mau.fi/mautrix-meta v0.0.0
)

require (
	github.com/andybalholm/brotli v1.2.4 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/icholy/digest v1.2.0 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.61.0 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	go.mau.fi/util v0.10.2-0.20260918225449-a4c0d5b86aa8 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace go.mau.fi/mautrix-meta => ../../bridge-e2ee/meta

replace github.com/imroc/req/v3 => github.com/beeper/req/v3 v3.0.0-20260925135712-b49593750e56
