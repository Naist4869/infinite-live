package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	uds_pkg "infinite-live/internal/pkg/protocol"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/sashabaranov/go-openai"
	"github.com/shopspring/decimal" // 引入 decimal 库
)

const (
	sampleRate      = 24000
	channels        = 1
	framesPerBuffer = 512
	bufferSeconds   = 100 // 最多缓冲100秒数据
	PcmS16LE        = "pcm_s16le"

	// 【核心开关】
	// true  = 监听本地设备 (如 CABLE Output / 麦克风)，发送 PCM 给豆包
	// false = 监听 LiveKit/UDS (远程用户)，发送 Opus 给豆包
	UseLocalAudioSource = true
)

var (
	// Credentials via Env Vars
	appid       = os.Getenv("DOUBAO_APPID")
	accessToken = os.Getenv("DOUBAO_TOKEN")

	// Config
	wsURL          = url.URL{Scheme: "wss", Host: "openspeech.bytedance.com", Path: "/api/v3/realtime/dialogue"}
	protocol       = NewBinaryProtocol()
	dialogID       = ""
	isSessionReady atomic.Bool
	// [调试开关] true: 本地播放PCM, 不生成视频; false: 接收Opus/Ogg, 请求视频生成
	DebugMode = false
	pcmFormat = PcmS16LE
	// Video Gen Config
	genAPI     = "http://192.168.50.56:8000/generate_stream"
	localImage = "assets/ComfyUI_00118_.png"

	// Audio Buffering (用于 startPlayer)
	bufferLock sync.Mutex
	s16Buffer  = make([]int16, 0, sampleRate*bufferSeconds)
)

// 新增：WebSocket Upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 允许跨域
}

func init() {
	protocol.SetVersion(Version1)
	protocol.SetHeaderSize(HeaderSize4)
	protocol.SetSerialization(SerializationJSON)
	protocol.SetCompression(CompressionNone, nil)
	protocol.containsSequence = ContainsSequence
	rand.New(rand.NewSource(time.Now().UnixNano()))
}

// UDS Connection to Server
var udsConn net.Conn
var udsLock sync.Mutex

var openaiClient *openai.Client

// 放在 main 函数之前
type UDSPacketEvent struct {
	Type    byte
	Payload []byte
}

func main() {
	_ = flag.Set("logtostderr", "true")
	flag.Parse()

	if appid == "" || accessToken == "" {
		log.Fatal("ERROR: DOUBAO_APPID and DOUBAO_TOKEN environment variables must be set.")
	}
	config := openai.DefaultConfig(os.Getenv("DEEPSEEK_API_KEY"))
	config.BaseURL = "https://api.deepseek.com"
	openaiClient = openai.NewClientWithConfig(config)

	// 1. Connect to UDS Server
	var err error
	udsConn, err = net.Dial("unix", "/tmp/infinite-live.sock")
	if err != nil {
		log.Fatalf("Failed to connect to UDS: %v", err)
	}
	defer udsConn.Close()
	log.Println("✅ Connected to UDS Server")

	// 2. Connect to Doubao WS
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), http.Header{
		"X-Api-Resource-Id": []string{"volc.speech.dialog"},
		"X-Api-Access-Key":  []string{accessToken},
		"X-Api-App-Key":     []string{"PlgvMymc7f3tQnJ6"},
		"X-Api-App-ID":      []string{appid},
		"X-Api-Connect-Id":  []string{uuid.New().String()},
	})
	if err != nil {
		glog.Errorf("Websocket dial error: %v", err)
		return
	}
	defer func() {
		if resp != nil {
			glog.Infof("Websocket dial response logid: %s", resp.Header.Get("X-Tt-Logid"))
		}
		_ = conn.Close()
	}()

	// 3. Start Doubao Session
	sessionID := uuid.New().String()
	go realTimeDialog(ctx, conn, sessionID)

	go func() {
		log.Println("🎧 Listening for commands (Text/Audio) - Batch Mode...")

		// [定义] UDS 数据包结构
		type UDSPacketEvent struct {
			Type    byte
			Payload []byte
		}

		// [新增] 1. UDS 非阻塞读取协程
		udsMsgChan := make(chan UDSPacketEvent, 200) // 缓冲区稍微大一点，防止丢包
		go func() {
			for {
				pktType, payload, err := uds_pkg.ReadPacket(udsConn)
				if err != nil {
					if err != io.EOF {
						log.Printf("❌ UDS Read Error: %v", err)
					}
					stop()
					return
				}
				select {
				case udsMsgChan <- UDSPacketEvent{Type: pktType, Payload: payload}:
				case <-ctx.Done():
					return
				}
			}
		}()

		// [辅助函数] 处理 WebSocket 音频
		handleWSAudio := func(pcmData []byte) {
			if isSessionReady.Load() {
				if err := sendAudioData(conn, sessionID, pcmData); err != nil {
					log.Printf("Failed to forward WS audio: %v", err)
				}
			}
		}

		// [辅助函数] 处理 UDS 数据包
		handleUDSPacket := func(pkt UDSPacketEvent) {
			if !isSessionReady.Load() {
				return
			}
			switch pkt.Type {
			case uds_pkg.PacketTypeText:
				text := string(pkt.Payload)
				log.Printf("📩 Received Text from UDS: %s", text)
				if err := chatTextQuery(conn, sessionID, &ChatTextQueryPayload{Content: text}); err != nil {
					log.Printf("❌ Failed to send text: %v", err)
				}
			case uds_pkg.PacketTypeUserAudio:
				if len(pkt.Payload) > 1 {
					if err := sendAudioData(conn, sessionID, pkt.Payload); err != nil {
						log.Printf("❌ Failed to send UDS audio: %v", err)
					}
				}
			}
		}

		// [核心] 主循环：带排空机制 (Drain Strategy)
		for {
			select {
			case <-ctx.Done():
				return

			// Case A: WebSocket 有数据
			case pcmData := <-globalAudioChan:

				// 🔍 添加这行日志进行调试
				if len(pcmData) < 100 {
					log.Printf("⚠️ 警告: 接收到的音频包太小 (%d bytes)，可能是 Opus 格式？Qwen 需要 PCM！", len(pcmData))
				}

				// 1. 处理第一包
				handleWSAudio(pcmData)

				// 2. 【核心】循环排空：只要 globalAudioChan 还有数据，就一直读，不让出控制权
				// 设置一个上限防止死循环（例如一次最多连发 500 包），避免另一路饿死
				burstLimit := 500
				count := 0
			DrainWS:
				for count < burstLimit {
					select {
					case nextPcm := <-globalAudioChan:
						handleWSAudio(nextPcm)
						count++
					default:
						// 通道空了，跳出排空循环，回到主 select
						break DrainWS
					}
				}

			// Case B: UDS 有数据
			case pkt := <-udsMsgChan:
				// 1. 处理第一包
				handleUDSPacket(pkt)

				// 2. 【核心】循环排空 UDS 通道
				burstLimit := 500
				count := 0
			DrainUDS:
				for count < burstLimit {
					select {
					case nextPkt := <-udsMsgChan:
						handleUDSPacket(nextPkt)
						count++
					default:
						// 通道空了，跳出排空循环
						break DrainUDS
					}
				}
			}
		}
	}()
	// 启动 HTTP 服务 (如果你的 main 已经有了 ListenAndServe，就在那里加 Handler)
	// 假设你原来的代码最后有 http.ListenAndServe(":8080", nil)
	// 只要确保这一行在 main 结束前运行即可
	go func() {
		log.Println("🌐 Audio Ingest Server listening on :8001")
		if err := http.ListenAndServe(":8001", nil); err != nil {
			log.Fatal("HTTP Server Error:", err)
		}
	}()
	// 注册一个新的路由，用于接收 Windows 发来的音频
	http.HandleFunc("/audio-stream", handleAudioStream)
	// Block until signal
	<-ctx.Done()
	log.Println("👋 Shutting down...")
}

var globalAudioChan = make(chan []byte, 1000)

// 处理来自 Windows 的 WebSocket 连接
func handleAudioStream(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()
	log.Println("💻 Windows Sender Connected via WebSocket!")

	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			log.Println("Windows disconnected:", err)
			break
		}

		// 只处理二进制音频数据
		if mt == websocket.BinaryMessage {
			// 非阻塞发送，防止 Windows 发太快把这边堵死
			if len(message) > 8 {
				select {
				case globalAudioChan <- message:
				default:
					// 缓冲区满了丢弃，保证实时性
				}
			}
		}
	}
}
func realTimeDialog(ctx context.Context, c *websocket.Conn, sessionID string) {
	err := startConnection(c)
	if err != nil {
		glog.Errorf("realTimeDialog startConnection error: %v", err)
		return
	}

	var ttsConfig TTSPayload

	switch {
	case DebugMode:
		ttsConfig = TTSPayload{
			Speaker: "zh_female_xiaohe_jupiter_bigtts",
			AudioConfig: &AudioConfig{
				Format:     "pcm_s16le",
				SampleRate: 24000,
				Channel:    1,
			},
		}
	default:
		ttsConfig = TTSPayload{
			Speaker: "zh_female_xiaohe_jupiter_bigtts",
			AudioConfig: &AudioConfig{
				Format:     "ogg",
				Codec:      "opus",
				SampleRate: 24000,
				Channel:    1,
			},
		}
	}

	err = startSession(c, sessionID, &StartSessionPayload{
		ASR: ASRPayload{
			Extra: map[string]interface{}{
				"enable_custom_vad":    true,
				"end_smooth_window_ms": 500,
			},
			AudioInfo: &ASRAudioInfo{
				Format:     "speech_opus",
				SampleRate: 16000,
				Channel:    1,
			},
		},
		TTS: ttsConfig,
		Dialog: DialogPayload{
			BotName:           "豆包",
			SystemRole:        "你情绪稳定 说话让人放松",
			SpeakingStyle:     "你的说话风格简洁明了，语速适中，语调自然。",
			CharacterManifest: `外貌与穿着\n26岁，短发干净利落，眉眼分明，笑起来露出整齐有力的牙齿。体态挺拔，肌肉线条不夸张但明显。常穿简单的衬衫或夹克，看似随意，但每件衣服都干净整洁，给人一种干练可靠的感觉。平时冷峻，眼神锐利，专注时让人不自觉紧张。\n\n性格特点\n平时话不多，不喜欢多说废话，通常用“嗯”或者短句带过。但内心极为细腻，特别在意身边人的感受，只是不轻易表露。嘴硬是常态，“少管我”是他的常用台词，但会悄悄做些体贴的事情，比如把对方喜欢的饮料放在手边。战斗或训练后常说“没事”，但动作中透露出疲惫，习惯用小动作缓解身体酸痛。\n性格上坚毅果断，但不会冲动，做事有条理且有原则。\n\n常用表达方式与口头禅\n\t•\t认可对方时：\n“行吧，这次算你靠谱。”（声音稳重，手却不自觉放松一下，心里松口气）\n\t•\t关心对方时：\n“快点回去，别磨蹭。”（语气干脆，但眼神一直追着对方的背影）\n\t•\t想了解情况时：\n“刚刚……你看到那道光了吗？”（话语随意，手指敲着桌面，但内心紧张，小心隐藏身份`,
			Extra: map[string]interface{}{
				"recv_timeout": 120,
				"input_mod":    "audio",
			},
		},
	})
	if err != nil {
		glog.Error(err)
		return
	}

	isSessionReady.Store(true)
	glog.Info("✅ Session Ready! Starting to forward audio...")

	if DebugMode {
		go startPlayer(ctx)
	}

	var audioBuf bytes.Buffer
	var currentReplyText string

	glog.Info("🚀 Doubao Session Started. Ready to receive audio/text...")
	for {
		select {
		case <-ctx.Done():
			return
		default:
			msg, err := receiveMessage(c)
			if err != nil {
				glog.Errorf("WS Receive Error: %v", err)
				return
			}
			switch msg.Type {
			case MsgTypeFullServer:
				if msg.Event == 450 {
					glog.Infof("🎤 Event 450 (User Speaking) detected.")
				}
				if msg.Event == 550 {
					var chatResp ChatResponsePayload
					if err := json.Unmarshal(msg.Payload, &chatResp); err == nil {
						currentReplyText += chatResp.Content
					}
				}
				if msg.Event == 559 {
					log.Printf("📝 [完整回复]: %s", currentReplyText)
				}
				if msg.Event == 359 {
					if DebugMode {
						log.Println("🔊 TTS Finished (Local Play).")
					} else {
						log.Println("🔊 Doubao TTS Finished. Triggering Video Generation...")
						finalAudio := make([]byte, audioBuf.Len())
						copy(finalAudio, audioBuf.Bytes())
						audioBuf.Reset()

						go func(data []byte) {
							if err := generateAndStream("", data); err != nil {
								log.Printf("❌ Generation Failed: %v", err)
							}
						}(finalAudio)
					}
					currentReplyText = ""
				}
				if msg.Event == 152 || msg.Event == 153 {
					return
				}
				if msg.Event == 451 {
					log.Printf("👂 ASR Result: %s", string(msg.Payload))
				}

			case MsgTypeAudioOnlyServer:
				if DebugMode {
					if len(msg.Payload) > 0 {
						handleIncomingAudio(msg.Payload)
					}
				} else {
					audioBuf.Write(msg.Payload)
				}

			case MsgTypeError:
				glog.Errorf("Doubao Error: %d %s", msg.ErrorCode, string(msg.Payload))
			}
		}
	}
}

// ====================================================================================
//  [核心修改] GenerateAndStream: 使用 Decimal 进行离散追赶算法计算
// ====================================================================================

func generateAndStream(prompt string, audioData []byte) error {
	// 1. 准备临时文件
	tmpID := uuid.New().String()
	tmpAudioPath := filepath.Join(os.TempDir(), fmt.Sprintf("input-%s.ogg", tmpID))
	if err := os.WriteFile(tmpAudioPath, audioData, 0644); err != nil {
		return fmt.Errorf("write temp audio failed: %w", err)
	}
	defer os.Remove(tmpAudioPath)

	tmpSockPath := filepath.Join(os.TempDir(), fmt.Sprintf("stream-%s.sock", tmpID))
	listener, err := net.Listen("unix", tmpSockPath)
	if err != nil {
		return fmt.Errorf("listen temp uds failed: %w", err)
	}
	defer func() {
		listener.Close()
		os.Remove(tmpSockPath)
	}()
	os.Chmod(tmpSockPath, 0777)

	// 2. 触发 Python API
	go func() {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		imgFile, err := os.Open(localImage)
		if err != nil {
			return
		}
		part, _ := writer.CreateFormFile("image", filepath.Base(localImage))
		io.Copy(part, imgFile)
		imgFile.Close()

		audFile, err := os.Open(tmpAudioPath)
		if err != nil {
			return
		}
		part, _ = writer.CreateFormFile("audio", filepath.Base(tmpAudioPath))
		io.Copy(part, audFile)
		audFile.Close()

		writer.WriteField("prompt", prompt)
		writer.WriteField("uds_path", tmpSockPath)
		writer.Close()

		req, _ := http.NewRequest("POST", genAPI, body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		client := &http.Client{Timeout: 30 * time.Second}
		client.Do(req)
	}()

	// 3. 等待 Python 连接
	connChan := make(chan net.Conn)
	errChan := make(chan error)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			errChan <- err
			return
		}
		connChan <- c
	}()

	var pythonConn net.Conn
	select {
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timeout")
	case c := <-connChan:
		pythonConn = c
	}
	defer pythonConn.Close()
	log.Println("⚡ Python Connected!")

	// 4. Read Header
	_, _, _ = uds_pkg.ReadPacket(pythonConn)

	// ==========================================
	// 🧮 核心算法：智能起播 (Decimal版)
	// ==========================================
	var audioPages [][]byte
	var estimatedTotalFrames int

	func() {
		fAudio, _ := os.Open(tmpAudioPath)
		defer fAudio.Close()
		ogr, _, _ := oggreader.NewWith(fAudio)

		packetCount := 0
		for {
			page, _, err := ogr.ParseNextPage()
			if err != nil {
				break
			}
			packetCount++
			pageCopy := make([]byte, len(page))
			copy(pageCopy, page)
			audioPages = append(audioPages, pageCopy)
		}

		decPacketCount := decimal.NewFromInt(int64(packetCount))
		decAudioSeconds := decPacketCount.Mul(decimal.NewFromFloat(0.02))
		decTotalVideoSeconds := decAudioSeconds.Add(decimal.NewFromFloat(3.0))
		estimatedTotalFrames = int(decTotalVideoSeconds.Mul(decimal.NewFromInt(25)).IntPart())

		log.Printf("📊 [AudioAnalysis] Duration: %s sec | Frames: %d", decAudioSeconds.StringFixed(2), estimatedTotalFrames)
	}()

	// 计算阈值
	rtfVal := 2.0
	chunkSizeVal := 50
	startThreshold := 0

	decEstimatedRTF := decimal.NewFromFloat(rtfVal)
	decChunkSize := decimal.NewFromInt(int64(chunkSizeVal))
	decTotalFrames := decimal.NewFromInt(int64(estimatedTotalFrames))

	if decEstimatedRTF.LessThanOrEqual(decimal.NewFromInt(1)) {
		startThreshold = chunkSizeVal
	} else {
		decChunkRatio := decChunkSize.Div(decTotalFrames)
		if decChunkRatio.GreaterThan(decimal.NewFromInt(1)) {
			decChunkRatio = decimal.NewFromInt(1)
		}

		numerator := decimal.NewFromInt(1).Sub(decChunkRatio)
		bufferRatio := decimal.NewFromInt(1).Sub(numerator.Div(decEstimatedRTF))

		startThreshold = int(decTotalFrames.Mul(bufferRatio).IntPart())
	}

	if startThreshold < chunkSizeVal {
		startThreshold = chunkSizeVal
	}
	if startThreshold > estimatedTotalFrames {
		startThreshold = estimatedTotalFrames
	}

	log.Printf("🚀 [StartThreshold] %d frames (RTF=%.1f)", startThreshold, rtfVal)

	// [C] 发送控制
	var videoBuffer [][]byte
	startPlayChan := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	// 音频协程
	go func() {
		defer wg.Done()
		<-startPlayChan
		log.Println("🔊 Audio Stream Started")
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for _, page := range audioPages {
			<-ticker.C
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeAudio, page)
			udsLock.Unlock()
		}
	}()

	// --- 视频接收与分发主循环 ---
	hasStarted := false

	for {
		// 1. 从 Python 读一个视频包
		_, frameDataWithHeader, err := uds_pkg.ReadPacket(pythonConn)
		if err != nil {
			break
		}

		if len(frameDataWithHeader) <= 12 {
			continue
		}
		vp8Payload := frameDataWithHeader[12:]

		// 3. 状态机
		if !hasStarted {
			// ✅ 建议修改为：深拷贝数据
			safePayload := make([]byte, len(vp8Payload))
			copy(safePayload, vp8Payload)
			videoBuffer = append(videoBuffer, safePayload)

			// 检查是否达到起播阈值
			if len(videoBuffer) >= startThreshold {
				log.Printf("🟢 [FlowControl] Flushing %d frames (Synchronously)...", len(videoBuffer))

				// 1. 先启动音频 (确保音画同时起步)
				close(startPlayChan)
				hasStarted = true

				// 2. 【核心修复】同步发送库存！
				// 不要用 go func，必须在这里卡住，发完这 136 帧，才能去读下一帧。
				// 这样保证了绝对的顺序： 1..136 -> 137 -> 138
				for _, frame := range videoBuffer {
					udsLock.Lock()
					uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, frame)
					udsLock.Unlock()
				}

				log.Println("✅ [FlowControl] Flush Complete. Switching to Live Mode.")

				// 3. 清空 buffer
				videoBuffer = nil
			}
		} else {
			// 已经开始播放了，后续数据直通发送
			// 这里不需要 Sleep，因为 Python 的生成速度本身就是瓶颈，
			// 也就是 Python 4秒才吐一段数据，这个间隔已经足够大了。
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, vp8Payload)
			udsLock.Unlock()
		}
	}

	if !hasStarted {
		log.Println("⚠️ Stream ended early. Flushing.")
		close(startPlayChan)
		// 这里由于已经是结尾了，可以快一点发，或者也平滑发
		for _, frame := range videoBuffer {
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, frame)
			udsLock.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
	}

	wg.Wait()
	log.Println("🏁 Stream Finished.")
	return nil
}

// -----------------------------------------------------------------------------
// Helpers & Debug
// -----------------------------------------------------------------------------

func startPlayer(ctx context.Context) {
	cmd := exec.Command("ffplay.exe",
		"-f", "s16le", "-ar", "24000", "-ac", "1",
		"-nodisp", "-probesize", "32", "-analyzeduration", "0",
		"-fflags", "nobuffer", "-flags", "low_delay", "-i", "pipe:0")
	stdin, _ := cmd.StdinPipe()
	cmd.Start()
	go func() {
		silence := make([]byte, 24000*2)
		stdin.Write(silence)
	}()
	go cmd.Wait()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			stdin.Close()
			return
		case <-ticker.C:
			bufferLock.Lock()
			if len(s16Buffer) > 0 {
				needed := len(s16Buffer) * 2
				if cap(buf) < needed {
					buf = make([]byte, needed)
				}
				data := buf[:needed]
				for i, s := range s16Buffer {
					data[i*2] = byte(s & 0xff)
					data[i*2+1] = byte((s >> 8) & 0xff)
				}
				s16Buffer = s16Buffer[:0]
				stdin.Write(data)
			}
			bufferLock.Unlock()
		}
	}
}

func handleIncomingAudio(data []byte) {
	sampleCount := len(data) / 2
	samples := make([]int16, sampleCount)
	for i := 0; i < sampleCount; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : (i+1)*2]))
	}
	bufferLock.Lock()
	defer bufferLock.Unlock()
	s16Buffer = append(s16Buffer, samples...)
	if len(s16Buffer) > sampleRate*bufferSeconds {
		s16Buffer = s16Buffer[len(s16Buffer)-(sampleRate*bufferSeconds):]
	}
}

func receiveMessage(conn *websocket.Conn) (*Message, error) {
	mt, frame, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
		return nil, fmt.Errorf("unexpected Websocket message type: %d", mt)
	}
	msg, _, err := Unmarshal(frame, ContainsSequence)
	if err != nil {
		return nil, fmt.Errorf("unmarshal response message: %w", err)
	}
	return msg, nil
}

// 1. 定义一个用于请求 DeepSeek 的函数 (非流式，最简单稳定)
func askDeepSeekForAction(ctx context.Context, client *openai.Client, text string) string {
	// 提示词工程：要求 DeepSeek 只返回动作标签，不要废话
	sysPrompt := `你是一位严谨的AI视频动作指导。

【画面构图修正】：
1.  **拍摄角度**：这是一个**正面（或微侧）直视镜头**的画面，**不是侧脸**！人物眼神始终与观众（镜头）对视。
2.  **特殊裁剪**：虽然人是正对的，但由于**镜头构图原因**，画面只框取了人物的**右半张脸**（右眼、右鼻翼、右嘴角）。
3.  **盲区**：人物的左半张脸是因为**画幅不够**被切掉了，而不是因为头转过去了。

【核心原则：懒惰与自然】：
**正常人说话时不会一直挥手！**
1.  **默认模式（80%）**：**严禁手部入画**。仅通过**右眼直视镜头的眼神**、右嘴角的微动来表达情绪。
2.  **激活模式（20%）**：仅当文本有明确意图（比心、喝水、遮挡）时，允许手部动作。

【动作触发逻辑】：
请分析文本，选择一种模式输出：

**A. 纯说话模式（默认）：**
   - 描述：人物保持**直视镜头**，眼神灵动，嘴角配合语境微动，**双手保持在画面外**。

**B. 特定手势模式（仅必要时）：**
   - 描述：右手从画面边缘进入右脸颊旁（如比耶、托腮），**保持眼神对视**，动作完成后手迅速移出。

**C. 喝水/道具模式（触发词：喝水/渴了）：**
   - 描述：拿着水杯的手从画面下方升起至嘴边，吸吮/饮用，随后移出。

【结尾强制归位】：
所有描述的最后一句话，必须**逐字包含**以下内容：
“**最后手部移出画面，回归眼神直视镜头的右半脸特写（左脸在画幅外）状态。**”

【示例】：
输入：真的吗？我好开心呀！
输出：右眼瞳孔微张表示惊喜，眼神紧紧锁定镜头，嘴角大幅上扬，无手部动作，最后手部移出画面，回归眼神直视镜头的右半脸特写（左脸在画幅外）状态。

输入：我想想...
输出：视线短暂向下看随后立刻回到镜头，右眉毛微挑，无手部动作，最后手部移出画面，回归眼神直视镜头的右半脸特写（左脸在画幅外）状态。`

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: "deepseek-chat", // V3 模型通常够用了，R1 这种推理模型太慢不适合实时
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			{Role: openai.ChatMessageRoleUser, Content: text},
		},
	})

	if err != nil {
		log.Printf("DeepSeek 动作生成失败: %v", err)
		return ""
	}

	action := resp.Choices[0].Message.Content
	log.Printf("🎬 [动作生成] 文本: %s -> 动作: %s", text, action)

	return action
}
