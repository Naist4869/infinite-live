package main

import (
	"encoding/binary"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall" // 引入 syscall 以支持更多信号
	"time"

	"github.com/gen2brain/malgo"
	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
)

// ==========================================
// 配置区域
// ==========================================

const (
	RemoteURL = "ws://192.168.50.56:8001/audio-stream"
	LocalURL  = "ws://192.168.50.169:8001/audio-stream"
)

const LocalAudioDeviceName = "CABLE Input"

const (
	SampleRate  = 16000
	Channels    = 1
	FrameSizeMs = 20
)

const FrameSizeSamples = SampleRate * FrameSizeMs / 1000

// ==========================================

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// 1. 解析参数
	formatPtr := flag.String("format", "opus", "Audio format: 'opus' or 'pcm'")
	serverPtr := flag.String("server", "remote", "Server target: 'local', 'remote' or full WS URL")
	flag.Parse()

	audioMode := strings.ToLower(*formatPtr)
	if audioMode != "opus" && audioMode != "pcm" {
		log.Fatal("❌ Unknown format. Use 'opus' or 'pcm'")
	}

	var targetURL string
	serverMode := strings.ToLower(*serverPtr)
	switch serverMode {
	case "local":
		targetURL = LocalURL
	case "remote":
		targetURL = RemoteURL
	default:
		if strings.HasPrefix(serverMode, "ws://") || strings.HasPrefix(serverMode, "wss://") {
			targetURL = *serverPtr
		} else {
			targetURL = RemoteURL
		}
	}

	log.Println("========================================")
	log.Printf("🎛️  Audio Mode  : %s", strings.ToUpper(audioMode))
	log.Printf("🌍 Server      : %s", targetURL)
	log.Println("========================================")

	// 2. 信号监听 (支持 Ctrl+C 和 kill 命令)
	interrupt := make(chan os.Signal, 1)
	// 监听 SIGINT (Ctrl+C) 和 SIGTERM (Docker stop / kill)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	// 控制变量
	var isRunning = true   // 标记程序是否运行中
	var ws *websocket.Conn // WebSocket 连接
	var wsLock sync.Mutex  // 保护 WS 的并发读写

	// 3. 初始化 Opus (仅 Opus 模式)
	var enc *opus.Encoder
	var err error
	if audioMode == "opus" {
		enc, err = opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
		if err != nil {
			log.Fatal("❌ Opus Encoder Init Failed:", err)
		}
		_ = enc.SetBitrate(24000)
	}

	// 4. 初始化 Malgo
	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		log.Fatal("❌ Malgo Init Failed:", err)
	}
	defer func() { _ = mctx.Uninit(); mctx.Free() }()

	// 配置设备
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Loopback)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = Channels
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1

	infos, _ := mctx.Devices(malgo.Playback)
	var selectedID *malgo.DeviceID
	foundName := ""

	log.Println("🔍 Scanning PLAYBACK Devices...")
	for _, info := range infos {
		if strings.Contains(info.Name(), LocalAudioDeviceName) {
			log.Printf("🎤 Match Found: %s", info.Name())
			selectedID = &info.ID
			foundName = info.Name()
			break
		}
	}
	log.Println(foundName)
	if selectedID != nil {
		deviceConfig.Capture.DeviceID = selectedID.Pointer()
	} else {
		log.Println("⚠️ Target device not found, using SYSTEM DEFAULT.")
	}

	// 5. WebSocket 连接逻辑
	connect := func() bool {
		wsLock.Lock()
		defer wsLock.Unlock()

		// 如果程序正在退出，不再重连
		if !isRunning {
			return false
		}

		u, _ := url.Parse(targetURL)
		log.Printf("🔌 Connecting to %s ...", u.String())
		c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Printf("❌ Connection failed: %v", err)
			return false
		}
		ws = c
		log.Println("✅ Connected to Server!")
		return true
	}

	// 初始连接
	go func() {
		for isRunning {
			wsLock.Lock()
			currentWs := ws
			wsLock.Unlock()

			if currentWs == nil {
				if connect() {
					// 连接成功，跳出循环，等待断开
				} else {
					time.Sleep(3 * time.Second)
					continue
				}
			}
			time.Sleep(1 * time.Second)
		}
	}()

	// 6. 音频回调
	var pcmBuffer []int16
	bufferMutex := sync.Mutex{}
	opusOut := make([]byte, 4000)

	onRecv := func(pOutputSample, pInputSamples []byte, framecount uint32) {
		// 如果程序已标记停止，直接返回，不再处理
		if !isRunning {
			return
		}

		if len(pInputSamples) == 0 {
			return
		}

		numSamples := len(pInputSamples) / 2
		inputInt16 := make([]int16, numSamples)
		for i := 0; i < numSamples; i++ {
			inputInt16[i] = int16(binary.LittleEndian.Uint16(pInputSamples[i*2 : i*2+2]))
		}

		bufferMutex.Lock()
		pcmBuffer = append(pcmBuffer, inputInt16...)

		for len(pcmBuffer) >= FrameSizeSamples {
			frame := pcmBuffer[:FrameSizeSamples]
			var packet []byte

			if audioMode == "opus" {
				n, err := enc.Encode(frame, opusOut)
				if err != nil {
					pcmBuffer = pcmBuffer[FrameSizeSamples:]
					continue
				}
				packet = make([]byte, n)
				copy(packet, opusOut[:n])
			} else {
				packet = make([]byte, len(frame)*2)
				for i, val := range frame {
					binary.LittleEndian.PutUint16(packet[i*2:], uint16(val))
				}
			}

			// 发送逻辑 (加锁保护)
			wsLock.Lock()
			if ws != nil {
				err := ws.WriteMessage(websocket.BinaryMessage, packet)
				if err != nil {
					log.Println("❌ Send Error:", err)
					ws.Close()
					ws = nil // 置空，触发重连逻辑
				}
			}
			wsLock.Unlock()

			pcmBuffer = pcmBuffer[FrameSizeSamples:]
		}
		bufferMutex.Unlock()
	}

	// 7. 启动音频设备
	deviceCallbacks := malgo.DeviceCallbacks{Data: onRecv}
	device, err := malgo.InitDevice(mctx.Context, deviceConfig, deviceCallbacks)
	if err != nil {
		log.Fatal("❌ Device Init Failed:", err)
	}
	if err := device.Start(); err != nil {
		log.Fatal("❌ Device Start Failed:", err)
	}

	log.Println("🚀 Streaming Started. Press Ctrl+C to stop.")

	// 8. 阻塞等待退出信号
	sig := <-interrupt
	log.Printf("\n⚠️  Received signal: %v. Shutting down...", sig)

	// ==========================================
	// 优雅退出逻辑
	// ==========================================

	// A. 标记程序停止 (防止重连协程继续工作)
	isRunning = false

	// B. 停止音频采集 (防止新的数据进入 onRecv)
	if device.IsStarted() {
		device.Stop()
	}
	device.Uninit()
	log.Println("🛑 Audio Capture Stopped")

	// C. 关闭 WebSocket
	wsLock.Lock()
	if ws != nil {
		// 发送 WebSocket 关闭控制帧 (Close Frame)
		// 这是一种礼貌的关闭方式，告诉服务器我们不是崩了，是正常退出
		err := ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client Exiting"))
		if err != nil {
			log.Println("⚠️  Close frame send error:", err)
		}
		ws.Close()
		ws = nil
	}
	wsLock.Unlock()
	log.Println("🛑 WebSocket Closed")

	log.Println("👋 Bye Bye!")
}
