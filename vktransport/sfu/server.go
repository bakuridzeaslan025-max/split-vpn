package sfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/pion/webrtc/v4"

	"github.com/bakuridzeaslan025-max/split-vpn/vktransport/platform"
)

// Server joins a VK call and accepts connections from clients via DataChannel.
type Server struct {
	ph platform.Handler
}

// NewServer creates a new SFU server that uses the given platform handler for signaling.
func NewServer(ph platform.Handler) *Server {
	return &Server{ph: ph}
}

// Listen joins the VK call and returns a net.Listener that yields DataChannel connections.
func (s *Server) Listen(ctx context.Context, callID string) (net.Listener, error) {
	if err := s.ph.Authorize(callID, ""); err != nil {
		return nil, fmt.Errorf("sfu server authorize: %w", err)
	}

	if err := s.ph.Connect(); err != nil {
		return nil, fmt.Errorf("sfu server signaling connect: %w", err)
	}

	if err := s.ph.NotifyVideoStream(true); err != nil {
		return nil, fmt.Errorf("sfu server notify video stream: %w", err)
	}

	lCtx, cancel := context.WithCancel(ctx)
	ln := &listener{
		acceptCh: make(chan net.Conn, 16),
		ctx:      lCtx,
		cancel:   cancel,
	}

	go s.acceptLoop(lCtx, ln)
	return ln, nil
}

func (s *Server) acceptLoop(ctx context.Context, ln *listener) {
	events := s.ph.WatchEvents(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == platform.EventCallEnded {
				slog.Info("sfu server: call ended")
				ln.cancel()
				return
			}
			if ev.Type == platform.EventRemoteMediaUpdated {
				sdpOffer := s.ph.GetRemoteMediaSDP()
				if sdpOffer == "" {
					continue
				}
				go s.handleOffer(ctx, ln, sdpOffer)
			}
		}
	}
}

func (s *Server) handleOffer(ctx context.Context, ln *listener, sdpOffer string) {
	iceServers := turnInfoToICEServers(s.ph.GetTURNInfo())

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		slog.Warn("sfu server create peer connection failed", "error", err)
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		slog.Warn("sfu server set remote description failed", "error", err)
		_ = pc.Close()
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		slog.Warn("sfu server create answer failed", "error", err)
		_ = pc.Close()
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		slog.Warn("sfu server set local description failed", "error", err)
		_ = pc.Close()
		return
	}

	if err := s.ph.SendAcceptProducer(answer.SDP); err != nil {
		slog.Warn("sfu server send accept-producer failed", "error", err)
		_ = pc.Close()
		return
	}
	slog.Debug("sfu server sent accept-producer")

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			slog.Info("sfu server data channel open", "label", dc.Label())
			conn := newConn(dc, pc)
			select {
			case ln.acceptCh <- conn:
			case <-ctx.Done():
				_ = conn.Close()
			}
		})
	})
}

type listener struct {
	acceptCh chan net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.acceptCh:
		if !ok {
			return nil, errors.New("listener closed")
		}
		return conn, nil
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

func (l *listener) Close() error {
	l.cancel()
	return nil
}

func (l *listener) Addr() net.Addr {
	return sfuAddr{}
}
