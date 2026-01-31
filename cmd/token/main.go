package main

import (
	"fmt"
	"os"
	"time"

	"github.com/livekit/protocol/auth"
)

func main() {
	apiKey := os.Getenv("LIVEKITAPIKEY")
	apiSecret := os.Getenv("LIVEKITSECRET")
	LiveKitURL := os.Getenv("LIVEKITURL")

	roomName := "infinite-live-room"

	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.AddGrant(grant).SetIdentity("viewer-test").SetValidFor(time.Hour * 24)

	token, _ := at.ToJWT()
	fmt.Println("LiveKit URL: ", LiveKitURL)
	fmt.Println("Token:", token)
}
