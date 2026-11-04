package main

import (
	"context"
	"flag"
	"log"

	"github.com/bakuridzeaslan025-max/split-vpn/vktransport"
	vkplatform "github.com/bakuridzeaslan025-max/split-vpn/vktransport/platform/vk"
	"github.com/bakuridzeaslan025-max/split-vpn/vktransport/sfu"
)

func main() {
	callID := flag.String("call-id", "", "VK call ID")
	server := flag.String("server", "", "UDP address of DTLS server")
	listen := flag.String("listen", "127.0.0.1:1080", "local TCP address for proxy")
	sfuMode := flag.Bool("sfu", false, "use SFU/DataChannel transport instead of DTLS")
	peerID := flag.String("peer-id", "", "server peer ID in the VK call (SFU mode)")
	name := flag.String("name", "vkt-client", "display name in the VK call")
	flag.Parse()

	if *callID == "" {
		log.Fatal("--call-id is required")
	}
	if !*sfuMode && *server == "" {
		log.Fatal("--server is required in DTLS mode")
	}

	if *sfuMode {
		ph := &vkplatform.Handler{}
		if err := ph.Authorize(*callID, *name); err != nil {
			log.Fatalf("authorize: %v", err)
		}
		client := sfu.NewClient(ph)
		serverPeer := *peerID
		log.Printf("SFU connecting to call %s (peer %s)", *callID, serverPeer)

		conn, err := client.Connect(context.Background(), *callID, serverPeer)
		if err != nil {
			log.Fatalf("sfu connect: %v", err)
		}
		defer conn.Close()

		log.Println("SFU connected via DataChannel")
		_ = listen
	} else {
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
}
