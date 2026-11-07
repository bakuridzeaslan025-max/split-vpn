package vkfallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	socksVersion  = 5
	socksUserPass = 2
	socksConnect  = 1
	socksIPv4     = 1
	socksDomain   = 3
	socksIPv6     = 4
)

var errSocksAuth = errors.New("vkfallback: local SOCKS refused the credentials")

// socksDial opens addr through the SOCKS5 server on the loopback port. olcrtc
// answers the CONNECT only after the exit has reached addr, so success proves
// the whole path.
func socksDial(ctx context.Context, port int, user, pass, addr string) (net.Conn, error) {
	request, err := socksRequest(addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err = socksHandshake(conn, user, pass, request)
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socksRequest(addr string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("vkfallback: port in %q: %w", addr, err)
	}
	req := []byte{socksVersion, socksConnect, 0}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		if len(host) == 0 || len(host) > 255 {
			return nil, fmt.Errorf("vkfallback: bad host in %q", addr)
		}
		req = append(req, socksDomain, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, socksIPv4)
		req = append(req, ip.To4()...)
	default:
		req = append(req, socksIPv6)
		req = append(req, ip.To16()...)
	}
	return append(req, byte(port>>8), byte(port)), nil
}

// socksHandshake reads no further than the reply: what follows belongs to the
// caller, who gets the bare connection.
func socksHandshake(conn net.Conn, user, pass string, request []byte) error {
	if _, err := conn.Write([]byte{socksVersion, 1, socksUserPass}); err != nil {
		return err
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		return err
	}
	if method != [2]byte{socksVersion, socksUserPass} {
		return errSocksAuth
	}
	auth := []byte{1, byte(len(user))}
	auth = append(auth, user...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, pass...)
	if _, err := conn.Write(auth); err != nil {
		return err
	}
	var status [2]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return err
	}
	if status[1] != 0 {
		return errSocksAuth
	}
	if _, err := conn.Write(request); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[1] != 0 {
		return fmt.Errorf("vkfallback: SOCKS CONNECT failed: reply %d", reply[1])
	}
	return socksSkipAddr(conn, reply[3])
}

func socksSkipAddr(conn net.Conn, addrType byte) error {
	var n int
	switch addrType {
	case socksIPv4:
		n = net.IPv4len
	case socksIPv6:
		n = net.IPv6len
	case socksDomain:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return err
		}
		n = int(length[0])
	default:
		return fmt.Errorf("vkfallback: SOCKS reply address type %d", addrType)
	}
	_, err := io.ReadFull(conn, make([]byte, n+2))
	return err
}
