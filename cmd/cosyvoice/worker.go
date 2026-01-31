package main

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"strings"
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
	localImage = "assets/share_5b8a5e532c2ae42359995de2f33b54a5~2.png"
)

var (
	isSessionReady atomic.Bool
	// [调试开关] true: 本地播放PCM, 不生成视频; false: 请求视频生成
	DebugMode = false
	// [新增调试开关] 是否保存 Worker 收到的音频为 OGG 文件
	DebugSaveOgg = false
	// Audio Buffering (用于 startPlayer 本地播放测试)
	bufferLock sync.Mutex
	s16Buffer  = make([]int16, 0, sampleRate*bufferSeconds)
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// UDS Connection to Server
var udsConn net.Conn
var udsLock sync.Mutex

// 修改全局通道的类型
var (
	globalAudioChan = make(chan []byte, 10000)
)

func main() {
	_ = flag.Set("logtostderr", "true")
	flag.Parse()

	// 1. Connect to UDS Server (渲染端)
	var err error
	udsConn, err = net.Dial("unix", "/tmp/infinite-live.sock")
	if err != nil {
		log.Fatalf("Failed to connect to UDS: %v", err)
	}
	defer udsConn.Close()
	log.Println("✅ Connected to UDS Server")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. Start Local Session
	go connectLocalServer(ctx)
	// =================================================================
	// 4. 生产者 A：UDS 读取循环 (LiveKit 音频源)
	// =================================================================
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
				if isSessionReady.Load() {
					globalAudioChan <- payload
				}

			}
		}
	}()

	// 启动 HTTP 服务用于接收 Windows 音频
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
		if mt == websocket.BinaryMessage && len(message) > 0 {
			if isSessionReady.Load() {
				globalAudioChan <- message
			}
		}
	}
}

// ====================================================================================
//  [核心修改] 连接本地 Python Server (server.py)
// ====================================================================================

func connectLocalServer(ctx context.Context) {
	// ---------------------------------------------------------
	// 🐛 调试：初始化 OGG Writer
	// ---------------------------------------------------------
	var debugOggWriter *oggwriter.OggWriter
	var rtpSeq uint16 = 0
	var rtpTimestamp uint32 = 0

	if DebugSaveOgg {
		log.Println("🐛 Debug Mode: Recording received audio to 'debug_worker_received.ogg'")
		// 48000 是 Opus 的标准采样率，1 是通道数
		writer, err := oggwriter.New("debug_worker_received.ogg", 48000, 1)
		if err != nil {
			log.Printf("❌ Failed to create debug ogg file: %v", err)
		} else {
			debugOggWriter = writer
			defer writer.Close()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("🔌 Connecting to Local Server: %s ...", LocalWSHost)
		conn, _, err := websocket.DefaultDialer.Dial(LocalWSHost, nil)
		if err != nil {
			log.Printf("❌ Connect failed: %v, retrying in 3s...", err)
			time.Sleep(3 * time.Second)
			continue
		}

		log.Println("✅ Connected to Local Brain (ASR/LLM/TTS)!")
		isSessionReady.Store(true)

		if DebugMode {
			go startPlayer(ctx)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// ---------------------------------------------------------
		// 1. 发送协程: globalAudioChan -> Python
		// ---------------------------------------------------------
		go func() {
			defer wg.Done()
			defer conn.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case dataToSend := <-globalAudioChan:
					// 这里不能把全部的opus流转发 因为消费端处理速度不够 所以要截断一些杂音匹配速度
					if len(dataToSend) < 4 {
						continue
					}

					// -------------------------------------------------
					// 🐛 调试：写入 OGG 文件
					// -------------------------------------------------
					if debugOggWriter != nil {
						// 我们需要构造一个假的 RTP 包，因为 OGG Writer 需要它
						// Opus 帧通常是 20ms，对应 48000 * 0.02 = 960 个采样点
						pkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    111, // Opus dynamic payload type
								SequenceNumber: rtpSeq,
								Timestamp:      rtpTimestamp,
								SSRC:           12345,
							},
							Payload: dataToSend,
						}

						if err := debugOggWriter.WriteRTP(pkt); err != nil {
							log.Printf("Debug write error: %v", err)
						}

						// 递增计数器 (模拟正常的时间流逝)
						rtpSeq++
						rtpTimestamp += 960
					}

					// 发送给 Python
					if err := conn.WriteMessage(websocket.BinaryMessage, dataToSend); err != nil {
						log.Printf("WS Write Error: %v", err)
						return
					}
				}
			}
		}()

		// ---------------------------------------------------------
		// 2. 接收协程 (保持不变)
		// ---------------------------------------------------------
		go func() {
			defer wg.Done()
			defer conn.Close()
			var audioBuf bytes.Buffer
			var currentAction string = "说话"

			for {
				mt, message, err := conn.ReadMessage()
				if err != nil {
					log.Printf("WS Read Error: %v", err)
					return
				}

				if mt == websocket.TextMessage {
					text := string(message)
					if strings.HasPrefix(text, "ACTION:") {
						currentAction = text[7:]
						log.Printf("🎬 [Action] %s", currentAction)
					} else if strings.HasPrefix(text, "TEXT:") {
						log.Printf("🤖 AI: %s", text[5:])
					} else if text == "CONTROL:INTERRUPT" {
						log.Println("🛑 Interrupted!")
						audioBuf.Reset()
						currentAction = "说话"
					} else if text == "CONTROL:TTS_END" {
						if DebugMode {
							log.Println("🔊 TTS End (Local Debug).")
						} else {
							log.Printf("🚀 Trigger Gen | Prompt: %s", currentAction)
							finalAudio := make([]byte, audioBuf.Len())
							copy(finalAudio, audioBuf.Bytes())
							go func(p string, a []byte) {
								if err := generateAndStream(p, a); err != nil {
									log.Printf("❌ Gen Failed: %v", err)
								}
							}(currentAction, finalAudio)
						}
						audioBuf.Reset()
						currentAction = "说话"
					}
				} else if mt == websocket.BinaryMessage {
					if DebugMode {
						handleIncomingAudio(message)
					} else {
						audioBuf.Write(message)
					}
				}
			}
		}()

		wg.Wait()
		isSessionReady.Store(false)
		log.Println("⚠️ Connection lost, reconnecting...")
	}
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
