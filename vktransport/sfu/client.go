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

// Client joins a VK call and connects to a peer (the server) via DataChannel.
type Client struct {
	ph platform.Handler
}

// NewClient creates a new SFU client that uses the given platform handler for signaling.
func NewClient(ph platform.Handler) *Client {
	return &Client{ph: ph}
}

// Connect joins the VK call, negotiates WebRTC via SFU, and returns a net.Conn over DataChannel.
func (c *Client) Connect(ctx context.Context, callID, serverPeerID string) (net.Conn, error) {
	if err := c.ph.Authorize(callID, ""); err != nil {
		return nil, fmt.Errorf("sfu client authorize: %w", err)
	}

	if err := c.ph.Connect(); err != nil {
		return nil, fmt.Errorf("sfu client signaling connect: %w", err)
	}

	cleanup := func() { c.ph.Disconnect() }

	if err := c.ph.NotifyVideoStream(true); err != nil {
		cleanup()
		return nil, fmt.Errorf("sfu client notify video stream: %w", err)
	}

	events := c.ph.WatchEvents(ctx)

	// wait for producer-updated SDP offer
	var sdpOffer string
	for {
		select {
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				cleanup()
				return nil, errors.New("sfu client: event channel closed")
			}
			if ev.Type == platform.EventCallEnded {
				reason := "unknown"
				if ev.Metadata != nil {
					if r := ev.Metadata["reason"]; r != "" {
						reason = r
					}
					if r := ev.Metadata["error"]; r != "" {
						reason = r
					}
				}
				cleanup()
				return nil, fmt.Errorf("sfu client: call ended: %s", reason)
			}
			if ev.Type == platform.EventRemoteMediaUpdated {
				sdpOffer = c.ph.GetRemoteMediaSDP()
				if sdpOffer == "" {
					continue
				}
				goto negotiate
			}
		}
	}

negotiate:
	iceServers := turnInfoToICEServers(c.ph.GetTURNInfo())

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("sfu client create peer connection: %w", err)
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateDisconnected ||
			state == webrtc.PeerConnectionStateClosed {
			_ = pc.Close()
		}
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		_ = pc.Close()
		cleanup()
		return nil, fmt.Errorf("sfu client set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		cleanup()
		return nil, fmt.Errorf("sfu client create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		cleanup()
		return nil, fmt.Errorf("sfu client set local description: %w", err)
	}

	if err := c.ph.SendAcceptProducer(answer.SDP); err != nil {
		_ = pc.Close()
		cleanup()
		return nil, fmt.Errorf("sfu client send accept-producer: %w", err)
	}
	slog.Debug("sfu client sent accept-producer")

	// wait for DataChannel
	dcCh := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			select {
			case dcCh <- dc:
			default:
			}
		})
	})

	select {
	case <-ctx.Done():
		_ = pc.Close()
		cleanup()
		return nil, ctx.Err()
	case dc := <-dcCh:
		slog.Info("sfu client data channel open", "label", dc.Label())
		return newConn(dc, pc), nil
	}
}
