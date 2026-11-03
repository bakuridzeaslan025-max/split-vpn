package main

import (
	"context"
	"flag"
	"log"

	"github.com/bakuridzeaslan025-max/split-vpn/vktransport"
)

func main() {
	callID := flag.String("call-id", "", "VK call ID")
	server := flag.String("server", "", "UDP address of DTLS server")
	listen := flag.String("listen", "127.0.0.1:1080", "local TCP address for proxy")
	flag.Parse()

	if *callID == "" || *server == "" {
		log.Fatal("--call-id and --server are required")
	}

	log.Printf("connecting to %s (call %s), local proxy on %s", *server, *callID, *listen)

	conn, err := vktransport.Dial(context.Background(), vktransport.ClientConfig{
		CallID:     *callID,
		ServerAddr: *server,
	})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	log.Println("connected")
}
