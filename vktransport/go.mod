module github.com/bakuridzeaslan025-max/split-vpn/vktransport

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/pion/dtls/v3 v3.1.2
	github.com/pion/logging v0.2.4
	github.com/pion/rtp v1.10.1
	github.com/pion/sdp/v3 v3.0.18
	github.com/pion/srtp/v3 v3.0.10
	github.com/pion/stun/v3 v3.1.2
	github.com/pion/turn/v5 v5.0.3
	github.com/useflyent/fhttp v0.0.0-20211004035111-333f430cfbbf
)

// fixes building on android locally inside termux
replace github.com/wlynxg/anet => github.com/BieHDC/anet v0.0.6-0.20241226223613-d47f8b766b3c

require (
	github.com/andybalholm/brotli v1.0.3 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
