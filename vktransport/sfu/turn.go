package sfu

import (
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
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{"turn:" + info.Address},
			Username:   info.Username,
			Credential: info.Password,
		})
	}
	return servers
}
