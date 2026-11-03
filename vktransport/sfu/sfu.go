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
	mu     sync.Mutex
	closed bool
	buf    []byte // buffered partial read
}

func newConn(dc *webrtc.DataChannel, pc *webrtc.PeerConnection) *Conn {
	c := &Conn{
		dc:     dc,
		pc:     pc,
		readCh: make(chan []byte, 256),
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := make([]byte, len(msg.Data))
		copy(data, msg.Data)
		select {
		case c.readCh <- data:
		default:
			// drop if buffer full
		}
	})
	return c
}

func (c *Conn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	data, ok := <-c.readCh
	if !ok {
		return 0, io.EOF
	}
	n := copy(b, data)
	if n < len(data) {
		c.mu.Lock()
		c.buf = append(c.buf, data[n:]...)
		c.mu.Unlock()
	}
	return n, nil
}

func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errors.New("connection closed")
	}
	c.mu.Unlock()

	if err := c.dc.Send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	close(c.readCh)
	_ = c.dc.Close()
	return c.pc.Close()
}

type sfuAddr struct{}

func (sfuAddr) Network() string { return "webrtc" }
func (sfuAddr) String() string  { return "sfu-datachannel" }

func (c *Conn) LocalAddr() net.Addr                { return sfuAddr{} }
func (c *Conn) RemoteAddr() net.Addr               { return sfuAddr{} }
func (c *Conn) SetDeadline(_ time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(_ time.Time) error { return nil }
