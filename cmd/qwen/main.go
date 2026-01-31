package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	uds_pkg "infinite-live/internal/pkg/protocol"
	"io"
	"log"
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

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sashabaranov/go-openai"
)

// =================配置区域=================
const (
	// Qwen 配置
	qwenModel     = "qwen3-omni-flash-realtime" // 或 "qwen-omni-turbo-realtime"
	qwenVoice     = "Kiki"                      // 音色
	dashScopeHost = "dashscope.aliyuncs.com"
	dashScopePath = "/api-ws/v1/realtime"

	// 音频配置
	inputSampleRate  = 16000 // Qwen 输入要求 PCM 16k
	outputSampleRate = 24000 // Qwen Flash 输出为 PCM 24k

	// 缓冲配置
	bufferSeconds = 100
)

var (
	apiKey = os.Getenv("DASHSCOPE_API_KEY")

	// 开关
	DebugMode           = false
	UseLocalAudioSource = true

	// 视频生成配置
	genAPI     = "http://192.168.50.56:8000/generate_stream"
	localImage = "assets/ComfyUI_00118_.png"

	// 状态控制
	isSessionReady atomic.Bool
	bufferLock     sync.Mutex
	s16Buffer      = make([]int16, 0, outputSampleRate*bufferSeconds)
)

// WebSocket Upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// UDS 连接
var udsConn net.Conn
var udsLock sync.Mutex

var openaiClient *openai.Client
var globalAudioChan = make(chan []byte, 1000)

// Qwen 协议结构体定义
type QwenEvent struct {
	Type       string                 `json:"type"`
	EventID    string                 `json:"event_id,omitempty"`
	Session    *QwenSessionConfig     `json:"session,omitempty"`
	Audio      string                 `json:"audio,omitempty"` // Base64 encoded
	Delta      string                 `json:"delta,omitempty"` // Base64 encoded (for output)
	Transcript string                 `json:"transcript,omitempty"`
	Error      map[string]interface{} `json:"error,omitempty"`
}

type QwenSessionConfig struct {
	Modalities        []string       `json:"modalities"`
	Voice             string         `json:"voice"`
	InputAudioFormat  string         `json:"input_audio_format"`
	OutputAudioFormat string         `json:"output_audio_format"`
	Instructions      string         `json:"instructions"`
	TurnDetection     *TurnDetection `json:"turn_detection,omitempty"`
}

type TurnDetection struct {
	Type              string  `json:"type"` // "server_vad" or null
	Threshold         float64 `json:"threshold,omitempty"`
	SilenceDurationMs int     `json:"silence_duration_ms,omitempty"`
}

type UDSPacketEvent struct {
	Type    byte
	Payload []byte
}

func init() {
	// 移除原有的 Doubao protocol init，因为不再需要复杂的二进制协议配置
}

func main() {
	_ = flag.Set("logtostderr", "true")
	flag.Parse()

	if apiKey == "" {
		log.Fatal("ERROR: DASHSCOPE_API_KEY environment variable must be set.")
	}

	// DeepSeek Client Init (保持不变)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. 启动 Qwen 对话主逻辑
	go qwenRealTimeDialog(ctx)

	// 3. 处理输入源 (WebSocket Audio + UDS)
	go inputHandler(ctx)

	// 4. HTTP Server for Windows Audio
	go func() {
		log.Println("🌐 Audio Ingest Server listening on :8001")
		http.HandleFunc("/audio-stream", handleAudioStream)
		if err := http.ListenAndServe(":8001", nil); err != nil {
			log.Fatal("HTTP Server Error:", err)
		}
	}()

	<-ctx.Done()
	log.Println("👋 Shutting down...")
}

// -----------------------------------------------------------------------------
// Qwen 核心逻辑
// -----------------------------------------------------------------------------

func qwenRealTimeDialog(ctx context.Context) {
	// 外层增加循环，实现断线重连
	for {
		// 检查上下文是否已取消（程序退出）
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Println("🔄 (Re)Connecting to Qwen Realtime API...")
		isSessionReady.Store(false) // 重连前标记不可用

		// 执行一次完整的会话周期
		runQwenSession(ctx)

		log.Println("⚠️ Session ended, retrying in 3 seconds...")
		time.Sleep(3 * time.Second)
	}
}

// 将原本的逻辑封装到 runQwenSession 中
func runQwenSession(ctx context.Context) {
	u := url.URL{Scheme: "wss", Host: dashScopeHost, Path: dashScopePath}
	q := u.Query()
	q.Set("model", qwenModel)
	u.RawQuery = q.Encode()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)

	// 设置握手超时
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, _, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		log.Printf("❌ Qwen Connection Failed: %v", err)
		return
	}
	defer conn.Close()

	log.Println("✅ Connected to Qwen")

	// --- 1. 发送 Session Update ---
	// Qwen Flash 强制要求 output_audio_format 为 pcm24
	sessionConfig := QwenSessionConfig{
		Modalities:        []string{"text", "audio"},
		Voice:             qwenVoice,
		InputAudioFormat:  "pcm16",
		OutputAudioFormat: "pcm24",
		Instructions:      "你的说话风格简洁明了，语速适中，语调自然。你情绪稳定 说话让人放松",
		TurnDetection: &TurnDetection{
			Type:              "server_vad",
			Threshold:         0.5,
			SilenceDurationMs: 800,
		},
	}

	initEvent := QwenEvent{
		Type:    "session.update",
		EventID: "evt_" + uuid.New().String(),
		Session: &sessionConfig,
	}

	if err := conn.WriteJSON(initEvent); err != nil {
		log.Printf("❌ Failed to send session update: %v", err)
		return
	}

	isSessionReady.Store(true)

	if DebugMode {
		go startPlayer(ctx)
	}

	// 用于通知发送协程退出的 channel
	closeSender := make(chan struct{})
	defer close(closeSender)

	// --- 2. 启动发送协程 ---
	go func() {
		log.Println("🚀 Audio sender routine started...")
		var packetCount int
		for {
			select {
			case <-closeSender: // 连接断开时退出
				return
			case <-ctx.Done():
				return
			case pcmData := <-globalAudioChan:
				if !isSessionReady.Load() {
					continue
				}

				packetCount++
				if packetCount%50 == 0 {
					log.Printf("📡 Sending Audio Packet #%d, Size: %d bytes", packetCount, len(pcmData))
				}

				b64 := base64.StdEncoding.EncodeToString(pcmData)
				evt := QwenEvent{
					Type:  "input_audio_buffer.append",
					Audio: b64,
				}

				wsWriteLock.Lock()
				// ⚠️ 关键修改：设置写超时，防止网络卡死
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteJSON(evt)
				wsWriteLock.Unlock()

				if err != nil {
					log.Printf("❌ Write Audio Error: %v", err)

					// ✅ 修改2: 发送失败时，主动关闭连接！
					// 这会让主线程的 ReadJSON 立即报错退出，从而触发重连逻辑
					conn.Close()
					return
				}
			}
		}
	}()

	var audioAccumulator bytes.Buffer
	var currentText string

	for {
		var msg QwenEvent
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("❌ Read Error (Connection Lost): %v", err)
			return // 退出，触发重连
		}

		// =====================================================
		// 🔍 全局日志打印 (过滤掉高频的音频/文本流数据)
		// =====================================================
		if msg.Type != "response.audio.delta" &&
			msg.Type != "response.audio_transcript.delta" {

			// 打印事件类型，如果有 EventID 也带上
			log.Printf("📨 [Event] %-35s | ID: %s", msg.Type, msg.EventID)
		}

		// =====================================================
		// ⚙️ 事件处理逻辑
		// =====================================================
		switch msg.Type {
		// 1. 会话状态类
		case "session.created":
			log.Println("🎉 Session Created")
		case "session.updated":
			log.Println("⚙️ Session Configured")

		// 2. 费率限制更新 (常见但容易被忽略)
		case "rate_limits.updated":
			// 这里通常包含 limit 和 remaining，debug 时很有用
			// log.Printf("💰 Rate Limits: %+v", msg)

		// 3. 输入音频缓冲类
		case "input_audio_buffer.speech_started":
			log.Println("🎤 User Started Speaking (Server VAD)")
			if DebugMode {
				bufferLock.Lock()
				s16Buffer = s16Buffer[:0]
				bufferLock.Unlock()
			}
			audioAccumulator.Reset()
			currentText = ""

		case "input_audio_buffer.speech_stopped":
			log.Println("🛑 User Stopped Speaking")

		case "input_audio_buffer.committed":
			log.Println("📦 Audio Buffer Committed") // 表示服务器已经切分了音频片段

		// 4. 对话项类
		case "conversation.item.created":
			// log.Println("💬 Conversation Item Created")

		case "conversation.item.input_audio_transcription.completed":
			// 用户的语音转文字结果
			log.Printf("📝 [User Transcript]: %s", msg.Transcript)

		case "conversation.item.input_audio_transcription.failed":
			log.Printf("⚠️ ASR Failed: %v", msg.Error)

		// 5. 响应类 (Response)
		case "response.created":
			log.Println("🤖 AI Response Generation Started")

		case "response.content_part.added":
			// log.Println("🧩 Content Part Added")

		case "response.audio.delta":
			// 接收音频流
			decoded, err := base64.StdEncoding.DecodeString(msg.Delta)
			if err != nil {
				continue
			}
			if DebugMode {
				handleIncomingAudio(decoded)
			} else {
				audioAccumulator.Write(decoded)
			}

		case "response.audio_transcript.delta":
			// 接收文本流
			currentText += msg.Delta
			// 如果你想看实时打字机效果，可以在这里 fmt.Print(msg.Delta)

		case "response.audio_transcript.done":
			log.Printf("📜 [AI Transcript]: %s", msg.Transcript)

		case "response.done":
			log.Println("✅ AI Response Finished")
			// 触发视频生成逻辑
			if !DebugMode && audioAccumulator.Len() > 0 {
				log.Println("🎬 Generating Video...")
				fullAudio := make([]byte, audioAccumulator.Len())
				copy(fullAudio, audioAccumulator.Bytes())
				audioAccumulator.Reset()

				go func(txt string, audio []byte) {
					action := askDeepSeekForAction(ctx, openaiClient, txt)
					if err := generateAndStream(action, audio); err != nil {
						log.Printf("❌ Generation Failed: %v", err)
					}
				}(currentText, fullAudio)

				currentText = ""
			}

		case "error":
			log.Printf("❌ Qwen API Error: %+v", msg.Error)

		// 6. 捕获所有未处理的事件
		default:
			log.Printf("⚠️ Unhandled Event Type: %s", msg.Type)
		}
	}
}

var wsWriteLock sync.Mutex

func inputHandler(ctx context.Context) {
	log.Println("👂 Starting Input Handler...")
	udsMsgChan := make(chan UDSPacketEvent, 200)

	// 读取 UDS
	go func() {
		for {
			// 这里假设 uds_pkg.ReadPacket 是阻塞的
			pktType, payload, err := uds_pkg.ReadPacket(udsConn)
			if err != nil {
				if err != io.EOF {
					log.Printf("❌ UDS Read Error: %v", err)
				}
				return
			}
			// 打印收到的包类型和大小
			// log.Printf("UDS Recv: Type=%d Len=%d", pktType, len(payload))
			udsMsgChan <- UDSPacketEvent{Type: pktType, Payload: payload}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		// 处理 UDS 文本/音频包
		case pkt := <-udsMsgChan:
			if pkt.Type == uds_pkg.PacketTypeUserAudio { // 假设 PacketTypeUserAudio 是音频
				if len(pkt.Payload) > 0 {
					// ⚠️⚠️⚠️ 关键检查点 ⚠️⚠️⚠️
					// 如果你的 UDS 传来的是 Opus 编码 (常见于 WebRTC/LiveKit)，这里必须解码成 PCM！
					// Qwen 只能接收 PCM。
					// 如果 UDS 传来的是 24k PCM，Qwen 需要 16k，你可能需要重采样（或者在 Qwen session update 里试试能不能设成 24k input，通常不可以）。

					// 暂时假设是 PCM，直接转发
					select {
					case globalAudioChan <- pkt.Payload:
					default:
						// 缓冲区满丢弃，防止阻塞
					}
				}
			} else if pkt.Type == uds_pkg.PacketTypeText {
				// 如果是文本输入，打印一下
				log.Printf("📩 Received Text Input: %s", string(pkt.Payload))
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 视频生成与流控 (PCM 版)
// -----------------------------------------------------------------------------

func generateAndStream(prompt string, audioData []byte) error {
	// 1. 准备临时文件
	tmpID := uuid.New().String()
	// 注意：我们要写入 wav 头，或者确保 Python 端能读 raw PCM (S16LE, 24k)
	// 为了兼容性，我们这里写入带 WAV 头的 PCM16 (已在接收端转换)
	tmpAudioPath := filepath.Join(os.TempDir(), fmt.Sprintf("input-%s.wav", tmpID))

	if err := writeWav(tmpAudioPath, audioData, 24000); err != nil {
		return fmt.Errorf("write wav failed: %w", err)
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

	// 2. 触发 Python API (保持不变)
	go func() {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		imgFile, err := os.Open(localImage)
		if err == nil {
			part, _ := writer.CreateFormFile("image", filepath.Base(localImage))
			io.Copy(part, imgFile)
			imgFile.Close()
		}

		// 发送 WAV 文件
		audFile, err := os.Open(tmpAudioPath)
		if err == nil {
			part, _ := writer.CreateFormFile("audio", filepath.Base(tmpAudioPath))
			io.Copy(part, audFile)
			audFile.Close()
		}

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
	go func() {
		c, err := listener.Accept()
		if err == nil {
			connChan <- c
		}
	}()

	var pythonConn net.Conn
	select {
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timeout waiting for python")
	case c := <-connChan:
		pythonConn = c
	}
	defer pythonConn.Close()

	// 读取 Header
	_, _, _ = uds_pkg.ReadPacket(pythonConn)

	// ==========================================
	// 🧮 核心算法：PCM 版流控
	// ==========================================
	// Qwen Flash (24k) 转成 PCM16 后:
	// 24000 samples/s * 2 bytes/sample = 48000 bytes/s
	// 20ms 一帧 = 48000 * 0.02 = 960 bytes

	chunkSize := 960 // 20ms chunks for 24k sample rate, 16bit
	totalBytes := len(audioData)
	estimatedDurationSec := float64(totalBytes) / 48000.0
	estimatedTotalFrames := int(estimatedDurationSec * 25.0) // 25fps video

	log.Printf("📊 [AudioAnalysis] Bytes: %d | Duration: %.2fs | Frames: %d", totalBytes, estimatedDurationSec, estimatedTotalFrames)

	// 切分音频块
	var audioChunks [][]byte
	for i := 0; i < totalBytes; i += chunkSize {
		end := i + chunkSize
		if end > totalBytes {
			end = totalBytes
		}
		// 拷贝一份数据
		chunk := make([]byte, end-i)
		copy(chunk, audioData[i:end])
		audioChunks = append(audioChunks, chunk)
	}

	// 计算起播阈值 (逻辑简化，沿用之前的思想)
	startThreshold := 50
	if estimatedTotalFrames < 50 {
		startThreshold = 10
	}

	// [C] 发送控制
	var videoBuffer [][]byte
	startPlayChan := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	// 音频发送协程
	go func() {
		defer wg.Done()
		<-startPlayChan // 等待视频缓冲就绪
		log.Println("🔊 Audio Stream Started")
		ticker := time.NewTicker(20 * time.Millisecond) // 20ms 发送一次音频
		defer ticker.Stop()

		for _, chunk := range audioChunks {
			<-ticker.C
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeAudio, chunk)
			udsLock.Unlock()
		}
	}()

	// 视频接收循环
	hasStarted := false
	for {
		_, frameDataWithHeader, err := uds_pkg.ReadPacket(pythonConn)
		if err != nil {
			break
		}
		if len(frameDataWithHeader) <= 12 {
			continue
		}
		vp8Payload := frameDataWithHeader[12:]

		if !hasStarted {
			safePayload := make([]byte, len(vp8Payload))
			copy(safePayload, vp8Payload)
			videoBuffer = append(videoBuffer, safePayload)

			if len(videoBuffer) >= startThreshold {
				log.Printf("🟢 [FlowControl] Flushing %d frames...", len(videoBuffer))

				// 启动音频
				close(startPlayChan)
				hasStarted = true

				// 倾泻视频缓冲
				for _, frame := range videoBuffer {
					udsLock.Lock()
					uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, frame)
					udsLock.Unlock()
					// 这里不需要sleep，因为是追赶
				}
				videoBuffer = nil
			}
		} else {
			// 直通
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, vp8Payload)
			udsLock.Unlock()
		}
	}

	// 处理未达阈值就结束的情况
	if !hasStarted {
		close(startPlayChan)
		for _, frame := range videoBuffer {
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, frame)
			udsLock.Unlock()
		}
	}

	wg.Wait()
	return nil
}

// -----------------------------------------------------------------------------
// 辅助函数
// -----------------------------------------------------------------------------

// writeWav 写一个简单的 WAV 头
func writeWav(filename string, pcmData []byte, sampleRate int) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	totalDataLen := uint32(len(pcmData))
	fileSize := totalDataLen + 36

	// RIFF Header
	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, fileSize)
	f.Write([]byte("WAVE"))

	// fmt chunk
	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16)) // Chunk size
	binary.Write(f, binary.LittleEndian, uint16(1))  // PCM
	binary.Write(f, binary.LittleEndian, uint16(1))  // Channels (Mono)
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	binary.Write(f, binary.LittleEndian, uint32(sampleRate*2)) // Byte Rate (16bit mono)
	binary.Write(f, binary.LittleEndian, uint16(2))            // Block Align
	binary.Write(f, binary.LittleEndian, uint16(16))           // Bits per sample

	// data chunk
	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, totalDataLen)
	f.Write(pcmData)

	return nil
}

func handleAudioStream(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()
	log.Println("💻 Windows Sender Connected")

	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage && len(message) > 0 {
			select {
			case globalAudioChan <- message:
			default:
			}
		}
	}
}

// handleIncomingAudio 存入 buffer 供本地播放
func handleIncomingAudio(data []byte) {
	// Input is PCM 16bit Little Endian
	sampleCount := len(data) / 2
	samples := make([]int16, sampleCount)
	for i := 0; i < sampleCount; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : (i+1)*2]))
	}
	bufferLock.Lock()
	defer bufferLock.Unlock()
	s16Buffer = append(s16Buffer, samples...)
	if len(s16Buffer) > outputSampleRate*bufferSeconds {
		s16Buffer = s16Buffer[len(s16Buffer)-(outputSampleRate*bufferSeconds):]
	}
}

func startPlayer(ctx context.Context) {
	// 使用 ffplay 播放 PCM 16bit 24k (因为我们转换过了)
	cmd := exec.Command("ffplay.exe",
		"-f", "s16le", "-ar", "24000", "-ac", "1",
		"-nodisp", "-probesize", "32", "-analyzeduration", "0",
		"-fflags", "nobuffer", "-flags", "low_delay", "-i", "pipe:0")

	stdin, _ := cmd.StdinPipe()
	cmd.Start()

	go func() {
		<-ctx.Done()
		stdin.Close()
	}()

	// 简单的播放循环
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bufferLock.Lock()
			if len(s16Buffer) > 0 {
				data := make([]byte, len(s16Buffer)*2)
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
