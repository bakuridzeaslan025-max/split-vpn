package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/bakuridzeaslan025-max/split-vpn/vktransport"
	vkplatform "github.com/bakuridzeaslan025-max/split-vpn/vktransport/platform/vk"
	"github.com/bakuridzeaslan025-max/split-vpn/vktransport/sfu"
)

func main() {
	listen := flag.String("listen", ":4461", "UDP address for DTLS listener")
	sfuMode := flag.Bool("sfu", false, "use SFU/DataChannel transport instead of DTLS")
	callID := flag.String("call-id", "", "VK call ID (SFU mode)")
	flag.Parse()

	if *sfuMode {
		if *callID == "" {
			log.Fatal("--call-id is required in SFU mode")
		}

		ph := &vkplatform.Handler{}
		srv := sfu.NewServer(ph)
		log.Printf("SFU listening on call %s", *callID)

		ln, err := srv.Listen(context.Background(), *callID)
		if err != nil {
			log.Fatalf("sfu listen: %v", err)
		}
		defer ln.Close()

		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("sfu accept: %v", err)
				return
			}
			go handleConn(conn)
		}
	} else {
		log.Printf("listening on %s", *listen)

		ln, err := vktransport.Listen(context.Background(), vktransport.ServerConfig{
			ListenAddr: *listen,
		})
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
		defer ln.Close()

		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("accept: %v", err)
				continue
			}
			go handleConn(conn)
		}
	}
}

// handleConn reads a 7-byte header [type][4 dst IP][2 dst port], dials the
// destination, and copies data bidirectionally.
func handleConn(src net.Conn) {
	defer src.Close()

	var hdr [7]byte
	if _, err := io.ReadFull(src, hdr[:]); err != nil {
		log.Printf("header read: %v", err)
		return
	}

	dstIP := net.IP(hdr[1:5])
	dstPort := binary.BigEndian.Uint16(hdr[5:7])
	addr := net.JoinHostPort(dstIP.String(), fmt.Sprintf("%d", dstPort))

	log.Printf("relay to %s (type %d)", addr, hdr[0])

	dst, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("dial %s: %v", addr, err)
		return
	}
	defer dst.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(dst, src)
		close(done)
	}()
	io.Copy(src, dst)
	<-done
}
