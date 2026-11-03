package sfu

import (
	"strings"

	"github.com/pion/webrtc/v4"

	protocol "github.com/bakuridzeaslan025-max/split-vpn/vktransport/proto"
)

// turnInfoToICEServers converts platform TURN credentials to pion ICE servers.
func turnInfoToICEServers(infos []protocol.TURNInfo) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(infos))
	for _, info := range infos {
		if info.Address == "" {
			continue
		}
		// Address is host:port after normalization; build a TURN URI for pion.
		url := info.Address
		if !strings.Contains(url, "://") && !strings.HasPrefix(url, "turn:") && !strings.HasPrefix(url, "turns:") {
			url = "turn:" + url + "?transport=udp"
		}
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{url},
			Username:   info.Username,
			Credential: info.Password,
		})
	}
	return servers
}
