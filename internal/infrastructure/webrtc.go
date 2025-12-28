package infrastructure

import (
	"github.com/pion/webrtc/v4"
)

// WebRTCManager handles the creation of PeerConnections
type WebRTCManager struct {
	api *webrtc.API
}

func NewWebRTCManager() *WebRTCManager {
	settingEngine := webrtc.SettingEngine{}
	
	// You might need to configure NAT 1:1 IPs here for production
	// settingEngine.SetNAT1To1IPs([]string{"1.2.3.4"}, webrtc.ICMPTypeCandidate)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	return &WebRTCManager{
		api: api,
	}
}

func (m *WebRTCManager) NewPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	return m.api.NewPeerConnection(config)
}
