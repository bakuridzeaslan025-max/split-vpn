package sfu

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Conn wraps a WebRTC DataChannel as net.Conn for tunneling.
type Conn struct {
	dc     *webrtc.DataChannel
	pc     *webrtc.PeerConnection
	readCh chan []byte
	done   chan struct{}
	mu     sync.Mutex // protects dc.Send and closed
	readMu sync.Mutex // serializes Read calls
	closed bool
	buf    []byte // buffered partial read
}

func newConn(dc *webrtc.DataChannel, pc *webrtc.PeerConnection) *Conn {
	c := &Conn{
		dc:     dc,
		pc:     pc,
		readCh: make(chan []byte, 256),
		done:   make(chan struct{}),
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := make([]byte, len(msg.Data))
		copy(data, msg.Data)
		select {
		case c.readCh <- data:
		case <-c.done:
		}
	})
	return c
}

func (c *Conn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	select {
	case data, ok := <-c.readCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			c.buf = data[n:]
		}
		return n, nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if err := c.dc.Send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	c.dc.Close()
	return c.pc.Close()
}

type sfuAddr struct{}

func (sfuAddr) Network() string { return "webrtc" }
func (sfuAddr) String() string  { return "sfu-datachannel" }

func (c *Conn) LocalAddr() net.Addr                { return sfuAddr{} }
func (c *Conn) RemoteAddr() net.Addr               { return sfuAddr{} }
func (c *Conn) SetDeadline(_ time.Time) error      { return errors.ErrUnsupported }
func (c *Conn) SetReadDeadline(_ time.Time) error  { return errors.ErrUnsupported }
func (c *Conn) SetWriteDeadline(_ time.Time) error { return errors.ErrUnsupported }
