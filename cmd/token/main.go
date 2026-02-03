package main

import (
	"fmt"
	"net/url" // 引入 url 包用于处理链接参数
	"os"
	"time"

	"github.com/livekit/protocol/auth"
)

func main() {
	// 1. 获取环境变量
	apiKey := os.Getenv("LIVEKITAPIKEY")
	apiSecret := os.Getenv("LIVEKITSECRET")
	liveKitURL := os.Getenv("LIVEKITURL") // 确保这里是 wss://... 格式

	roomName := "infinite-live-room"

	// 2. 生成 Token
	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.AddGrant(grant).SetIdentity("xiaosai").SetValidFor(time.Hour * 24)

	token, err := at.ToJWT()
	if err != nil {
		fmt.Printf("Error generating token: %v\n", err)
		return
	}

	// 3. 构建完整的加入链接
	// LiveKit Meet 的基础地址
	baseUrl := "https://meet.livekit.io/custom"

	// 使用 url.Values 构建查询参数，它会自动处理 URL 编码
	params := url.Values{}
	params.Add("liveKitUrl", liveKitURL)
	params.Add("token", token)

	// 拼接最终链接
	finalUrl := fmt.Sprintf("%s?%s", baseUrl, params.Encode())

	// 4. 输出结果
	fmt.Println("点击下方链接加入房间:")
	fmt.Println(finalUrl)
}
