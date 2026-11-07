package vkfallback

import (
	"errors"

	"github.com/openlibrecommunity/olcrtc/mobile"
)

const (
	defaultFPS = 30
	// olcrtc's own default; its constant is unexported.
	vp8BatchSize = 64
)

// engine is what fallback needs from olcrtc's mobile.Runtime.
type engine interface {
	Configure(room, keyHex string, fps int) error
	SetDNS(servers string) error
	SetProtector(p Protector)
	SetSessionHandler(fn func())
	Start(socksPort int, socksUser, socksPass string) error
	WaitReady(timeoutMillis int) error
	IsRunning() bool
	Stop(timeoutMillis int) error
}

type olcrtc struct{ *mobile.Runtime }

func newOlcrtc() *olcrtc { return &olcrtc{mobile.New()} }

func (o *olcrtc) Configure(room, keyHex string, fps int) error {
	if fps == 0 {
		fps = defaultFPS
	}
	// Dial only speaks CONNECT.
	o.SetUDP(false)
	return errors.Join(
		o.SetProvider("vkcalls"),
		o.SetTransport("vp8channel"),
		o.SetRoom(room),
		o.SetKey(keyHex),
		o.SetVP8Options(fps, vp8BatchSize),
	)
}

func (o *olcrtc) SetProtector(p Protector) { o.Runtime.SetProtector(p) }

type sessionFunc func()

func (fn sessionFunc) OnSessionOpened(string, string) { fn() }

func (o *olcrtc) SetSessionHandler(fn func()) { o.SetSessionListener(sessionFunc(fn)) }

func (o *olcrtc) Start(socksPort int, socksUser, socksPass string) error {
	if err := errors.Join(o.SetSocksPort(socksPort), o.SetSocksCredentials(socksUser, socksPass)); err != nil {
		return err
	}
	return o.Runtime.Start()
}
