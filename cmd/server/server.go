package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// Global references
var (
	currentInteractor *usecase.LiveInteractor
	broadcaster       *infrastructure.UDSBroadcaster
	LiveKitURL        = os.Getenv("LIVEKITURL")
	LiveKitAPIKey     = os.Getenv("LIVEKITAPIKEY")
	LiveKitSecret     = os.Getenv("LIVEKITSECRET")
	debugServerOgg    = false
)

// 配置信息 (建议放入环境变量)
const (
	RoomName      = "infinite-live-room"
	ParticipantID = "digital-human-bot"
)

func main() {
	log.Println("Starting InfiniteLive Core (LiveKit Edition)...")

	// =================================================================
	// 1. 初始化 LiveKit Room 回调 (修正版)
	// =================================================================
	roomCB := &lksdk.RoomCallback{
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) {
			log.Println("User disconnected:", p.Identity())
		},

		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Kind() != webrtc.RTPCodecTypeAudio {
					return
				}
				if rp.Identity() == ParticipantID {
					return
				}

				mimeType := strings.ToLower(track.Codec().MimeType)
				log.Printf("🎤 Audio Stream Start (JitterBuffer Mode): %s | Mime: %s", rp.Identity(), mimeType)

				// =======================================================
				// 1. 文件创建 (放在循环外面！)
				// =======================================================
				var debugOgg *oggwriter.OggWriter

				if debugServerOgg {
					// 建议加上时间戳，防止文件名冲突
					fileName := fmt.Sprintf("debug_server_%s_%d.ogg", rp.Identity(), time.Now().Unix())

					// 48000 是 Opus 标准采样率，1 是通道数
					writer, err := oggwriter.New(fileName, 48000, 1)
					if err != nil {
						log.Printf("❌ Failed to create debug ogg: %v", err)
					} else {
						debugOgg = writer
						log.Printf("🔴 Debug recording started: %s", fileName)
						// 确保函数退出时关闭文件
						defer debugOgg.Close()
					}
				}

				// 初始化 Jitter Buffer (排序器)
				sb := samplebuilder.New(50, &codecs.OpusPacket{}, 48000)

				// =======================================================
				// 2. 循环读取 (死循环)
				// =======================================================
				for {
					pkt, _, err := track.ReadRTP()
					if err != nil {
						log.Println("Audio track ended")
						break
					}

					// 推入排序器
					sb.Push(pkt)

					// 循环取出排好序的包
					for {
						sample := sb.Pop()
						if sample == nil {
							break
						}

						// =======================================================
						// 3. 写入文件 (放在循环里面！)
						// =======================================================
						if debugOgg != nil && len(sample.Data) > 0 {
							// 因为 SampleBuilder 返回的是 payload，我们需要伪造一个 RTP 包头给 OGG Writer 用
							// 重要的是 Payload 和 Timestamp
							fakeRTP := &rtp.Packet{
								Header: rtp.Header{
									Version:        2,
									PayloadType:    111,                    // Opus
									SequenceNumber: 0,                      // OGG Writer 其实不看这个
									Timestamp:      sample.PacketTimestamp, // 关键：使用排序后的时间戳
									SSRC:           12345,
								},
								Payload: sample.Data,
							}

							if err := debugOgg.WriteRTP(fakeRTP); err != nil {
								// 忽略写入错误，不要打断主流程
							}
						}
						if len(sample.Data) > 0 && broadcaster != nil {
							broadcaster.SendToWorker(protocol.PacketTypeUserAudio, sample.Data)
						}

					}
				}
			},
		},
	}

	// =================================================================
	// 2. 连接到 LiveKit 服务器 (后续代码完全不用动)
	// =================================================================
	room, err := lksdk.ConnectToRoom(LiveKitURL, lksdk.ConnectInfo{
		APIKey:              LiveKitAPIKey,
		APISecret:           LiveKitSecret,
		RoomName:            RoomName,
		ParticipantIdentity: ParticipantID,
	}, roomCB)

	// ... (以下代码保持原样) ...
	if err != nil {
		log.Fatalf("Failed to connect to LiveKit: %v", err)
	}
	defer room.Disconnect()

	log.Println("Connected to LiveKit Room:", room.Name())

	// ... 3. Video Track ...
	videoTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8,
		ClockRate: 90000,
	})
	if err != nil {
		log.Fatal(err)
	}

	// ... (Video bind/unbind) ...
	videoTrack.OnBind(func() { log.Println("Video Bound") })

	if _, err := room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{Name: "avatar_video"}); err != nil {
		log.Fatal(err)
	}

	// ... 4. Audio Track ...
	audioTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus,
	})
	if err != nil {
		log.Fatal(err)
	}

	if _, err := room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{Name: "avatar_audio"}); err != nil {
		log.Fatal(err)
	}

	// ... 5. 业务逻辑 ...
	idleSource, _ := file.NewLoopReader("./assets/idle.ivf", domain.StateIdle)

	idleAudioSource, _ := file.NewOggLoopReader("assets/idle.ogg")

	udsServer, _ := infrastructure.NewUDSServer("/tmp/infinite-live.sock")
	defer udsServer.Close()
	broadcaster = infrastructure.NewUDSBroadcaster(udsServer)
	broadcaster.Start()

	pktCh := broadcaster.Subscribe()
	talkingSource := uds.NewChannelSource(pktCh)

	lkPublisher := lkAdapter.NewLiveKitPublisher(videoTrack, audioTrack)

	interactor := usecase.NewLiveInteractor(lkPublisher, idleSource, idleAudioSource)
	interactor.SetTalkingSource(talkingSource)
	currentInteractor = interactor

	go interactor.StartLoop()

	// ... 7. HTTP ...
	http.HandleFunc("/comment", handleComment)
	http.HandleFunc("/token", handleToken)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Println("HTTP Server listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

// unwrapRED 解析 RFC 2198 包，剔除 Header 和冗余，只返回 Primary Payload
func unwrapRED(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, nil // 空包安全返回
	}

	var idx int
	var dataOffset int

	// 1. 遍历 Header，计算所有冗余数据的长度
	// 只要 F 位 (0x80) 是 1，说明后面还有冗余块描述
	for (payload[idx] & 0x80) != 0 {
		// 边界检查：防止畸形包导致 panic
		if idx+4 > len(payload) {
			return nil, nil
		}

		// 提取 Block Length (RFC 2198)
		blockLen := (int(payload[idx+2])&0x03)<<8 | int(payload[idx+3])

		dataOffset += blockLen
		idx += 4
	}

	// 2. 此时 idx 指向 Primary Header (1 byte, F=0)
	idx++

	// 3. 计算 Primary Data 的起始位置
	// Start = (Primary Header 结束位置) + (之前累加的所有冗余数据长度)
	payloadStart := idx + dataOffset

	if payloadStart >= len(payload) {
		return nil, nil // 数据不完整或只有头
	}

	// 返回剩余部分 (纯净的 Opus 帧)
	return payload[payloadStart:], nil
}

// handleComment 处理前端发来的文本评论
func handleComment(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	log.Printf("Received Comment: %s", string(body))

	// 发送给 Worker AI (Client)
	if broadcaster != nil {
		broadcaster.SendToWorker(protocol.PacketTypeText, body)
	}

	// 触发本地逻辑（如果有）
	if currentInteractor != nil {
		currentInteractor.OnUserComment(string(body))
	}
	w.WriteHeader(http.StatusOK)
}

// handleToken 生成 LiveKit Token
func handleToken(w http.ResponseWriter, r *http.Request) {
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
