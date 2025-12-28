package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"infinite-live/internal/adapter/file"
	lkAdapter "infinite-live/internal/adapter/livekit" // 引入新写的 adapter
	"infinite-live/internal/adapter/uds"
	"infinite-live/internal/domain"
	"infinite-live/internal/infrastructure"
	"infinite-live/internal/pkg/protocol"
	"infinite-live/internal/usecase"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

// Global references
var (
	currentInteractor *usecase.LiveInteractor
	broadcaster       *infrastructure.UDSBroadcaster
	LiveKitURL        = os.Getenv("LIVEKITURL")
	LiveKitAPIKey     = os.Getenv("LIVEKITAPIKEY")
	LiveKitSecret     = os.Getenv("LIVEKITSECRET")
)

// 配置信息 (建议放入环境变量)
const (
	RoomName      = "infinite-live-room"
	ParticipantID = "digital-human-bot"
)

func main() {
	log.Println("Starting InfiniteLive Core (LiveKit Edition)...")

	// 1. 初始化 LiveKit Room 回调
	roomCB := &lksdk.RoomCallback{
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) {
			log.Println("User disconnected:", p.Identity())
		},
	}

	// 2. 连接到 LiveKit 服务器
	room, err := lksdk.ConnectToRoom(LiveKitURL, lksdk.ConnectInfo{
		APIKey:              LiveKitAPIKey,
		APISecret:           LiveKitSecret,
		RoomName:            RoomName,
		ParticipantIdentity: ParticipantID,
	}, roomCB)
	if err != nil {
		log.Fatalf("Failed to connect to LiveKit: %v", err)
	}
	defer room.Disconnect()

	log.Println("Connected to LiveKit Room:", room.Name())

	// 3. 创建并发布 Video Track
	// H264 Capablity
	videoTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8, // <--- 关键修改
		ClockRate: 90000,
	})
	if err != nil {
		log.Fatal(err)
	}
	// 绑定回调，确认什么时候开始真正推流
	videoTrack.OnBind(func() {
		log.Println(">>> Video Track BOUND! Starting to send data...")
	})
	videoTrack.OnUnbind(func() {
		log.Println(">>> Video Track UNBOUND!")
	})
	// 发布 Video，设置 Simulcast 为 false 因为我们是直接推流文件，不需要多层编码
	if _, err := room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name: "avatar_video",
	}); err != nil {
		log.Fatal(err)
	}

	// 4. 创建并发布 Audio Track
	audioTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus,
	})
	if err != nil {
		log.Fatal(err)
	}

	if _, err := room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name: "avatar_audio",
	}); err != nil {
		log.Fatal(err)
	}

	// 5. 初始化业务逻辑
	// 准备资源
	idlePath := "./assets/idle.ivf" // 改为 ivf
	idleSource, err := file.NewLoopReader(idlePath, domain.StateIdle)
	if err != nil {
		log.Fatalf("Failed to create file source: %v", err)
	}
	// --- 加载音频 (新增) ---
	idleAudioPath := "assets/idle.ogg"
	// 使用刚才写的 NewOggLoopReader
	idleAudioSource, err := file.NewOggLoopReader(idleAudioPath)
	if err != nil {
		log.Fatalf("Audio init failed: %v (Did you run ffmpeg to generate .ogg?)", err)
	}
	// 初始化 UDS 广播器 (保持原有逻辑不变)
	udsServer, err := infrastructure.NewUDSServer("/tmp/infinite-live.sock")
	if err != nil {
		log.Fatalf("Failed to start UDS server: %v", err)
	}
	defer udsServer.Close()
	broadcaster = infrastructure.NewUDSBroadcaster(udsServer)
	broadcaster.Start()

	// 订阅 UDS
	pktCh := broadcaster.Subscribe()
	talkingSource := uds.NewChannelSource(pktCh)

	// 使用新的 LiveKit Publisher
	lkPublisher := lkAdapter.NewLiveKitPublisher(videoTrack, audioTrack)

	// 初始化 Interactor
	interactor := usecase.NewLiveInteractor(lkPublisher, idleSource, idleAudioSource)
	interactor.SetTalkingSource(talkingSource)
	currentInteractor = interactor

	// 6. 启动推流循环 (异步启动)
	// LiveKit 连接成功后，我们就可以一直推流，无论有没有用户在房间里
	// 如果希望没用户时不推流以节省 CPU，需要监听 OnParticipantConnected 事件
	go interactor.StartLoop()

	// 7. 启动 HTTP 服务 (用于前端页面和 Comment 接口)
	http.HandleFunc("/comment", handleComment)
	// 在 main 函数里注册
	http.HandleFunc("/token", handleToken)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Println("HTTP Server listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

// handleComment 保持不变，它是你的业务触发器
func handleComment(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	log.Printf("Received Comment: %s", string(body))

	// 发送给 Worker AI
	if broadcaster != nil {
		broadcaster.SendToWorker(protocol.PacketTypeText, body)
	}

	if currentInteractor != nil {
		currentInteractor.OnUserComment(string(body))
	}
	w.WriteHeader(http.StatusOK)
}

func handleToken(w http.ResponseWriter, r *http.Request) {
	// 创建一个 User Token
	at := auth.NewAccessToken(LiveKitAPIKey, LiveKitSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     RoomName,
	}
	// 随机生成一个用户 ID
	at.AddGrant(grant).SetIdentity("user-" + time.Now().String()).SetValidFor(time.Hour)

	token, err := at.ToJWT()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}
