package vktransport

import (
	"context"
	"net"
)

type CaptchaSolver func(url string) (string, error)

type ClientConfig struct {
	CallID     string
	ServerAddr string // UDP address of DTLS server
	Captcha    CaptchaSolver
}

type ServerConfig struct {
	ListenAddr string // UDP address
}

func Dial(ctx context.Context, cfg ClientConfig) (net.Conn, error) {
	// TODO: VK anonymous auth → TURN credentials → DTLS via TURN → net.Conn
	panic("not implemented")
}

func Listen(ctx context.Context, cfg ServerConfig) (net.Listener, error) {
	// TODO: DTLS listener
	panic("not implemented")
}
