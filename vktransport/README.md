# vktransport

Standalone VK TURN transport library, extracted from [Turnable](https://github.com/TheAirBlow/Turnable).

Provides anonymous VK authentication, TURN relay negotiation, and DTLS transport
as a reusable Go library with a `net.Conn` / `net.Listener` API.

## Directory structure

```
vktransport/
├── cmd/
│   ├── vkt-client/   CLI client: connects via VK TURN relay
│   └── vkt-server/   CLI server: accepts DTLS connections, relays TCP
├── common/           Shared utilities (profiles, resolver, URL helpers)
├── config/           Global options
├── platform/vk/      VK API: auth, signaling, captcha solving
├── proto/            Protocol handlers: DTLS, SRTP, TURN, masking
└── vktransport.go    Top-level Dial / Listen API
```

## Build

```bash
cd vktransport && go build ./...
```

## Usage (skeleton)

Server:
```bash
go run ./cmd/vkt-server --listen :4461
```

Client:
```bash
go run ./cmd/vkt-client --call-id XXX --server host:4461 --listen 127.0.0.1:1080
```

## Status

WIP. Relay mode skeleton only — `Dial` and `Listen` are stubs.
SFU / P2P mode is not yet implemented.

## Credit

Based on [github.com/TheAirBlow/Turnable](https://github.com/TheAirBlow/Turnable).
