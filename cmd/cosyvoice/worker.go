package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	uds_pkg "infinite-live/internal/pkg/protocol" // 请确保路径和你项目一致
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
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
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/shopspring/decimal"
	"gopkg.in/hraban/opus.v2"
)

const (
	// Local Server Config
	LocalWSHost = "wss://tts.lilis.ai/ws/chat"

	sampleRate    = 24000
	bufferSeconds = 100
	PcmS16LE      = "pcm_s16le"

	// Video Gen Config
	genAPI     = "http://192.168.50.56:8000/generate_stream"
	localImage = "assets/Screenshot_20260202_140824.jpg"
)

var (
	// [调试开关] true: 本地播放PCM, 不生成视频; false: 请求视频生成
	DebugMode = false
	// [新增调试开关] 是否保存 Worker 收到的音频为 OGG 文件
	DebugSaveOgg = false
	// Audio Buffering (用于 startPlayer 本地播放测试)
	bufferLock sync.Mutex
	s16Buffer  = make([]int16, 0, sampleRate*bufferSeconds)
)

// === 核心修改 1: 使用 Atomic Value 存储当前 Worker 的唯一 ID ===
// 之前是 isSessionReady atomic.Bool
var currentWorkerID atomic.Value // 存储 string

// 辅助函数：判断是否就绪
func isWorkerReady() bool {
	val := currentWorkerID.Load()
	return val != nil && val.(string) != ""
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// UDS Connection to Server
var udsConn net.Conn
var udsLock sync.Mutex

// 指令通道 (HTTP -> Python)
type WorkerCommand struct {
	Type string `json:"type"`           // "task.text", "control.interrupt"
	Text string `json:"text,omitempty"` // 文本内容
	Mode string `json:"mode,omitempty"` // "chat" 或 "tts"
}

// 修改全局通道的类型
var (
	globalAudioChan   = make(chan []byte, 10000)
	globalCommandChan = make(chan WorkerCommand, 100)
)

func main() {
	_ = flag.Set("logtostderr", "true")
	flag.Parse()
	// 初始化 currentWorkerID 为空字符串
	currentWorkerID.Store("")
	// 1. 连接 UDS
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error

	udsConn, err = net.Dial("unix", "/tmp/infinite-live.sock")
	if err != nil {
		log.Fatalf("Failed to connect to UDS: %v", err)
	}
	defer udsConn.Close()
	log.Println("✅ Connected to UDS Server")

	// 2. 监听 UDS 音频输入
	go func() {
		log.Println("🎧 Listening for UDS packets...")
		for {
			// 阻塞读取：由于下面采用了非阻塞发送，这里可以放心读，不用担心卡住
			pktType, payload, err := uds_pkg.ReadPacket(udsConn)
			if err != nil {
				if err != io.EOF {
					log.Printf("❌ UDS Read Error: %v", err)
				}
				stop()
				return
			}
			if pktType == uds_pkg.PacketTypeUserAudio && len(payload) > 0 {
				if isWorkerReady() {
					globalAudioChan <- payload
				}
			}
		}
	}()

	// 3. 注册路由
	// WebSocket: 接收音频流 (Windows)
	http.HandleFunc("/audio-stream", handleAudioStream)
	// WebSocket: Python Worker 连接
	http.HandleFunc("/worker/connect", func(w http.ResponseWriter, r *http.Request) {
		handleWorkerConnection(ctx, w, r)
	})
	// HTTP API: 提交任务
	http.HandleFunc("/task/chat", handleHttpTask("chat")) // 对话模式
	http.HandleFunc("/task/tts", handleHttpTask("tts"))   // 直接TTS模式

	log.Println("🚀 Central Controller listening on :8001")
	log.Println("   👉 Audio Stream (WS): ws://localhost:8001/audio-stream")
	log.Println("   👉 Worker Connect (WS): ws://localhost:8001/worker/connect")
	log.Println("   👉 Chat API (POST): http://localhost:8001/task/chat Body: {\"text\": \"...\"}")
	log.Println("   👉 TTS API (POST):  http://localhost:8001/task/tts  Body: {\"text\": \"...\"}")

	srv := &http.Server{Addr: ":8001"}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP Server Error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("👋 Shutting down...")
	srv.Shutdown(context.Background())
}

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
		if mt == websocket.BinaryMessage && len(message) > 0 && isWorkerReady() {
			globalAudioChan <- message

		}
	}
}

// 通用任务处理函数 (Chat / TTS)
func handleHttpTask(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type TaskRequest struct {
			Text string `json:"text"`
		}
		var req TaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if req.Text == "" {
			http.Error(w, "Text is required", http.StatusBadRequest)
			return
		}

		// 发送到指令通道
		cmd := WorkerCommand{
			Type: "task.text",
			Text: req.Text,
			Mode: mode,
		}

		// 非阻塞发送，防止通道满导致 HTTP 卡住
		select {
		case globalCommandChan <- cmd:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": mode})
		default:
			http.Error(w, "System busy", http.StatusServiceUnavailable)
		}
	}
}

// ====================================================================================
//  [核心修改] 连接本地 Python Server (server.py)
// ====================================================================================

// 处理 Python Worker 连接
func handleWorkerConnection(appCtx context.Context, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Worker upgrade failed: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(appCtx)
	defer cancel() // 确保退出时释放

	connRemoteAddr := conn.RemoteAddr().String()
	log.Printf("🤖 Python Worker Connected! [%s]", connRemoteAddr)
	// === 核心修改 3: 生成唯一 Session ID ===
	// 使用时间戳+远程端口作为简易ID，足以区分不同连接
	mySessionID := fmt.Sprintf("%d-%s", time.Now().UnixNano(), connRemoteAddr)

	log.Printf("🤖 Python Worker Connected! [%s] ID: %s", connRemoteAddr, mySessionID)

	// 标记为就绪
	currentWorkerID.Store(mySessionID)

	// 注册退出清理逻辑
	defer func() {
		conn.Close()
		// === 核心修改 4: 只有当当前ID等于我的ID时，才置空 ===
		// 这样防止旧连接退出时，把新连接的状态给冲掉了
		if currentWorkerID.Load() == mySessionID {
			currentWorkerID.Store("")
			log.Printf("⚠️ Worker Disconnected [%s] (State Cleared)", connRemoteAddr)
		} else {
			log.Printf("⚠️ Worker Disconnected [%s] (State Preserved for New Worker)", connRemoteAddr)
		}
	}()
	// === 核心修改 5: 心跳保活 (防止僵尸连接卡5分钟) ===
	// 设置 ReadDeadline，如果 60秒 内没有收到任何消息（包括 Ping/Pong），则认为连接已死
	// Python 端默认 ping_interval=20，所以这里设置 60 是安全的
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	// 设置 Ping 处理器：当收到 Python 发来的 Ping 时触发
	conn.SetPingHandler(func(appData string) error {
		log.Println("收到客户端 Ping，延长连接时间")
		// 延长 ReadDeadline
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		// 必须手动回复 Pong（或者调用默认 handler）
		// 简单做法是调用 WriteControl 回复 Pong，或者直接返回 nil
		// 注意：gorilla 的默认 PingHandler 会自动回 Pong，
		// 但如果我们覆盖了它，就需要确保 Pong 被发送，或者仅仅做延时处理。
		// 标准做法如下：
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		if err == nil {
			return nil
		}
		return err
	})
	if DebugMode {
		go startPlayer(ctx)
	}

	// OGG Debug Writer (略去详细实现，保持原样)
	var debugOggWriter *oggwriter.OggWriter
	if DebugSaveOgg {
		writer, _ := oggwriter.New("debug_worker_sent.ogg", 48000, 1)
		if writer != nil {
			debugOggWriter = writer
			defer writer.Close()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// --- A. 发送协程 (Go -> Python) ---
	go func() {
		defer wg.Done()
		defer conn.Close()
		var rtpSeq uint16 = 0
		var rtpTimestamp uint32 = 0

		for {
			select {
			case <-ctx.Done():
				return
			// 1. 发送音频
			case audioData := <-globalAudioChan:
				if len(audioData) < 4 {
					continue
				}
				if debugOggWriter != nil {
					pkt := &rtp.Packet{
						Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: rtpSeq, Timestamp: rtpTimestamp, SSRC: 12345},
						Payload: audioData,
					}
					debugOggWriter.WriteRTP(pkt)
					rtpSeq++
					rtpTimestamp += 960
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, audioData); err != nil {
					return
				}
			// 2. 发送指令
			case cmd := <-globalCommandChan:
				log.Printf("📤 Command -> Worker: [%s] %s", cmd.Mode, cmd.Text)
				if err := conn.WriteJSON(cmd); err != nil {
					return
				}
			}
		}
	}()

	// --- B. 接收协程 (Python -> Go) ---
	go func() {
		defer wg.Done()
		defer conn.Close()

		type WorkerResponse struct {
			Type   string `json:"type"`
			Delta  string `json:"delta,omitempty"` // Base64 Audio
			Text   string `json:"text,omitempty"`
			Action string `json:"action,omitempty"`
		}

		var audioBuf bytes.Buffer
		var currentAction string = "说话" // 默认动作

		for {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("❌ Worker Read Error: %v", err)
				return
			}
			// 收到任何消息都刷新 Deadline
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			if mt == websocket.TextMessage {
				var resp WorkerResponse
				if err := json.Unmarshal(message, &resp); err != nil {
					log.Printf("⚠️ Invalid JSON from worker: %v", err)
					continue
				}

				switch resp.Type {
				case "worker.register":
					log.Println("✅ Worker Registered Capabilities.")

				case "worker.detected_speech":
					log.Println("\n🛑 User started speaking (Interrupting...)")
					audioBuf.Reset()
					// 可以在这里通知前端停止播放

				case "response.text.ai":
					fmt.Printf("\n🤖 AI: %s\n", resp.Text)

				case "response.action":
					currentAction = resp.Action
					fmt.Printf("🎬 Action: %s\n", currentAction)

				case "response.audio.delta":
					// 1. 进度条 (打印点)
					fmt.Print(".")

					// 2. 解码 Base64 -> Binary
					if resp.Delta != "" {
						decoded, err := base64.StdEncoding.DecodeString(resp.Delta)
						if err != nil {
							log.Printf("❌ Base64 Decode Error: %v", err)
							continue
						}

						audioBuf.Write(decoded) // 存入 Buffer 等待生成视频

					}

				case "response.done":
					// 进度条结束换行
					fmt.Println("\n✅ Generation Done.")

					log.Printf("🚀 Trigger Video Gen | Prompt: %s | AudioSize: %d", currentAction, audioBuf.Len())

					if audioBuf.Len() > 0 {
						finalAudio := make([]byte, audioBuf.Len())
						copy(finalAudio, audioBuf.Bytes())
						if DebugMode {
							handleIncomingAudio(finalAudio) // 本地播放

						} else {

							go func(p string, a []byte) {
								if err := generateAndStream(p, a); err != nil {
									log.Printf("❌ Gen Failed: %v", err)
								}
							}(currentAction, finalAudio)
						}
						// 异步调用视频生成

					}

					// 重置状态
					audioBuf.Reset()
					currentAction = "说话"
				}
			}
		}
	}()

	wg.Wait()
	log.Println("⚠️ Worker Disconnected.")
}

// ====================================================================================
//  GenerateAndStream: 视频生成逻辑 (保持大部分不变，Prompt 改为参数传入)
// ====================================================================================

func generateAndStream(prompt string, audioData []byte) error {
	// 1. 智能检测音频格式 (WAV vs PCM)
	var fileData []byte // 用于保存到磁盘 (给视频生成用，必须带头)
	var pcmData []byte  // 用于流式传输 (给播放器用，必须去头)

	// 检查是否有 RIFF 头 (WAV 文件的标志)
	if len(audioData) > 44 && string(audioData[0:4]) == "RIFF" {
		log.Println("🔍 Detected input format: WAV (stripping header for stream)")
		fileData = audioData
		// 去掉 44 字节的标准 WAV 头，提取纯 PCM 数据用于播放
		// 注意：如果你的 WAV 头含有额外 metadata，可能不止 44 字节，但通常 TTS 生成的是 44
		pcmData = audioData[44:]
	} else {
		log.Println("🔍 Detected input format: Raw PCM (adding header for file)")
		pcmData = audioData
		// 加上 WAV 头保存，否则视频生成模型可能读不懂
		fileData = addWavHeader(audioData, 24000)
	}

	// ==========================================
	// 2. 准备临时文件 (给 Python 视频生成 API)
	// ==========================================
	tmpID := uuid.New().String()
	tmpAudioPath := filepath.Join(os.TempDir(), fmt.Sprintf("input-%s.wav", tmpID)) // 后缀改回 .wav

	// 写入带头的数据 (fileData)
	if err := os.WriteFile(tmpAudioPath, fileData, 0644); err != nil {
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

	go func() {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		imgFile, err := os.Open(localImage)
		if err != nil {
			log.Printf("❌ Failed to open image: %v", err)
			return
		}
		part, _ := writer.CreateFormFile("image", filepath.Base(localImage))
		io.Copy(part, imgFile)
		imgFile.Close()

		audFile, err := os.Open(tmpAudioPath)
		if err != nil {
			log.Printf("❌ Failed to open audio: %v", err)
			return
		}
		part, _ = writer.CreateFormFile("audio", filepath.Base(tmpAudioPath))
		io.Copy(part, audFile)
		audFile.Close()

		writer.WriteField("prompt", "她向观众说话。")
		writer.WriteField("uds_path", tmpSockPath)
		writer.Close()

		req, _ := http.NewRequest("POST", genAPI, body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		client := &http.Client{Timeout: 30 * time.Second}
		_, err = client.Do(req)
		if err != nil {
			log.Printf("❌ Video Gen API Error: %v", err)
		}
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
	const (
		AudioSampleRate = 24000
		Channels        = 1
		BytesPerSample  = 2
		FrameDurationMs = 20 // Opus 支持 2.5, 5, 10, 20, 40, 60 ms
	)
	// 计算每一帧需要的采样点数: 24000 * 0.04 = 960 samples
	const frameSizeSamples = AudioSampleRate * FrameDurationMs / 1000

	func() {
		// 1. 初始化 Opus 编码器
		enc, err := opus.NewEncoder(AudioSampleRate, Channels, opus.AppVoIP)
		if err != nil {
			log.Printf("❌ Opus Encoder Init Failed: %v", err)
			return
		}

		// 2. 将 []byte PCM 转换为 []int16 PCM
		// pcmData 是去除了 WAV 头的纯数据
		totalSamples := len(pcmData) / 2
		int16Buffer := make([]int16, totalSamples)
		for i := 0; i < totalSamples; i++ {
			// Little Endian 转换
			int16Buffer[i] = int16(binary.LittleEndian.Uint16(pcmData[i*2 : i*2+2]))
		}

		// 3. 计算用于视频同步的总时长 (逻辑不变)
		totalSeconds := float64(totalSamples) / float64(AudioSampleRate)
		estimatedTotalFrames = int((totalSeconds + 3.0) * 25)

		// 4. 分块编码
		// Opus 编码输出 buffer，通常 1000 字节足够容纳 40ms 的语音
		encodedBuf := make([]byte, 1000)

		for i := 0; i < len(int16Buffer); i += frameSizeSamples {
			end := i + frameSizeSamples
			var chunk []int16

			if end > len(int16Buffer) {
				// 处理最后一个不完整的包：补零 (Padding)
				// Opus 要求输入必须是允许的帧大小（如 960）
				chunk = make([]int16, frameSizeSamples)
				copy(chunk, int16Buffer[i:])
			} else {
				chunk = int16Buffer[i:end]
			}

			// 执行编码
			n, err := enc.Encode(chunk, encodedBuf)
			if err != nil {
				log.Printf("⚠️ Opus Encode Error: %v", err)
				continue
			}

			// 复制编码后的数据 (Opus Payload)
			opusPacket := make([]byte, n)
			copy(opusPacket, encodedBuf[:n])
			audioPages = append(audioPages, opusPacket)
		}

		log.Printf("📊 [Opus Encoded] RawBytes: %d | Duration: %.2fs | OpusPkts: %d", len(pcmData), totalSeconds, len(audioPages))
	}()

	// 计算阈值 (保持原逻辑)
	rtfVal := 2.0
	chunkSizeVal := 50
	startThreshold := 0
	if estimatedTotalFrames > 0 {
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
	} else {
		startThreshold = 50 // Fallback
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
				close(startPlayChan)
				hasStarted = true

				for _, frame := range videoBuffer {
					udsLock.Lock()
					uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, frame)
					udsLock.Unlock()
				}
				videoBuffer = nil
			}
		} else {
			udsLock.Lock()
			uds_pkg.WritePacket(udsConn, uds_pkg.PacketTypeVideo, vp8Payload)
			udsLock.Unlock()
		}
	}

	if !hasStarted {
		close(startPlayChan)
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
// Helpers & Debug (用于本地播放测试)
// -----------------------------------------------------------------------------

func startPlayer(ctx context.Context) {
	// 1. 确保使用绝对路径或系统能找到 ffplay.exe
	// WSL 能够直接运行 Path 中的 .exe，但建议检查一下
	path, err := exec.LookPath("ffplay.exe")
	if err != nil {
		fmt.Println("Error: ffplay.exe not found. Make sure ffmpeg/bin is in your Windows Path.")
		return
	}

	cmd := exec.Command(path,
		"-f", "s16le", // 格式
		"-ar", "24000", // 采样率
		"-ch_layout", "mono", // ✅ 修正点1：使用新版参数 -ch_layout mono 代替 -ac 1
		"-nodisp",          // 无窗口
		"-probesize", "32", // 低延迟优化
		"-analyzeduration", "0",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		// "-ac", "1",       // 如果 -ch_layout 依然不行，可以解开这行注释，但前提是必须删掉 -i
		"-", // ✅ 修正点2：使用 "-" 代表标准输入，比 "pipe:0" 在某些shell下更兼容，且绝对不能加 "-i"
	)

	// 2. 【关键】接管标准错误输出，否则 ffplay 报错你看不到
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Println("Error creating stdin pipe:", err)
		return
	}

	// 3. 启动命令
	if err := cmd.Start(); err != nil {
		fmt.Println("Error starting ffplay:", err)
		return
	}

	// 预先写入一点静音数据，防止 ffplay 启动初期因为没数据而立即退出或卡住
	go func() {
		silence := make([]byte, 24000*2) // 1秒静音
		_, err := stdin.Write(silence)
		if err != nil {
			// 如果这里报错，说明 ffplay 可能已经退出了
			fmt.Println("Error writing silence:", err)
		}
	}()

	// 监听进程退出
	go func() {
		err := cmd.Wait()
		fmt.Println("ffplay exited:", err)
	}()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	// 缓冲区复用
	buf := make([]byte, 4096)

	for {
		select {
		case <-ctx.Done():
			stdin.Close()
			// 最好显式杀掉进程，防止僵尸进程
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			return
		case <-ticker.C:
			bufferLock.Lock()
			// 4. 检查是否有数据
			if len(s16Buffer) > 0 {
				needed := len(s16Buffer) * 2
				if cap(buf) < needed {
					buf = make([]byte, needed)
				}
				data := buf[:needed]

				// 转换 int16 slice 到 byte slice (Little Endian)
				for i, s := range s16Buffer {
					data[i*2] = byte(s & 0xff)
					data[i*2+1] = byte((s >> 8) & 0xff)
				}

				// 清空缓冲区
				s16Buffer = s16Buffer[:0]

				// 解锁必须在 Write 之前吗？
				// 如果 Write 阻塞，会锁死生成端。
				// 建议先拷贝数据，解锁，再 Write。
				bufferLock.Unlock()

				// 写入数据到 ffplay
				_, err := stdin.Write(data)
				if err != nil {
					fmt.Println("Error writing audio data:", err)
					return // 管道破裂，退出循环
				}
			} else {
				bufferLock.Unlock()
			}
		}
	}
}

func handleIncomingAudio(data []byte) {
	// 假设 incoming 是 PCM S16LE
	sampleCount := len(data) / 2
	samples := make([]int16, sampleCount)
	for i := 0; i < sampleCount; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : (i+1)*2]))
	}
	bufferLock.Lock()
	defer bufferLock.Unlock()
	s16Buffer = append(s16Buffer, samples...)
}

// 辅助函数：如果未来需要强制加 WAV 头
func addWavHeader(data []byte, sampleRate int) []byte {
	totalDataLen := len(data)
	totalFileLen := totalDataLen + 36
	header := make([]byte, 44)

	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(totalFileLen))
	copy(header[8:12], []byte("WAVE"))

	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)

	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], uint32(totalDataLen))

	return append(header, data...)
}
