package main

import (
	"bytes"
	"context"
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
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

var (
	// Credentials via Env Vars
	appid       = os.Getenv("DOUBAO_APPID")
	accessToken = os.Getenv("DOUBAO_TOKEN")

	// Config
	wsURL    = url.URL{Scheme: "wss", Host: "openspeech.bytedance.com", Path: "/api/v3/realtime/dialogue"}
	protocol = NewBinaryProtocol()
	dialogID = ""

	// Video Gen Config
	genAPI     = "http://192.168.50.56:8000/generate_stream"
	localImage = "assets/IMG-20251126-WA0003.jpg"
)

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

func main() {
	_ = flag.Set("logtostderr", "true")
	flag.Parse()

	if appid == "" || accessToken == "" {
		log.Fatal("ERROR: DOUBAO_APPID and DOUBAO_TOKEN environment variables must be set.")
	}

	// 1. Connect to UDS Server
	var err error
	udsConn, err = net.Dial("unix", "/tmp/infinite-live.sock")
	if err != nil {
		log.Fatalf("Failed to connect to UDS: %v", err)
	}
	defer udsConn.Close()
	log.Println("Connected to UDS Server")

	// 2. Connect to Doubao WS
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), http.Header{
		"X-Api-Resource-Id": []string{"volc.speech.dialog"},
		"X-Api-Access-Key":  []string{accessToken},
		"X-Api-App-Key":     []string{"PlgvMymc7f3tQnJ6"}, // Fixed AppKey from doc/example
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

	// 3. Start Doubao Session (Receive Only Audio, No Mic Input)
	sessionID := uuid.New().String()
	go realTimeDialog(ctx, conn, sessionID)

	// 5. Listen for Text from UDS (Browser Comment -> UDS -> Here -> Doubao)
	go func() {
		log.Println("🎧 Listening for text commands from UDS...")
		for {
			pktType, payload, err := uds_pkg.ReadPacket(udsConn)
			if err != nil {
				if err != io.EOF {
					log.Printf("❌ UDS Read Error: %v", err)
				}
				stop() // UDS 断开通常意味着主程序挂了，我们也退出
				return
			}
			if pktType == uds_pkg.PacketTypeText {
				text := string(payload)
				log.Printf("📩 Received Text: %s", text)

				// Send to Doubao
				if err := chatTextQuery(conn, sessionID, &ChatTextQueryPayload{Content: text}); err != nil {
					log.Printf("❌ Failed to send text to Doubao: %v", err)
				}
			}
		}
	}()

	// Block until signal
	<-ctx.Done()
	log.Println("👋 Shutting down...")
}

func realTimeDialog(ctx context.Context, c *websocket.Conn, sessionID string) {
	err := startConnection(c)
	if err != nil {
		glog.Errorf("realTimeDialog startConnection error: %v", err)
		return
	}

	if err = startSession(c, sessionID, &StartSessionPayload{
		ASR: ASRPayload{
			Extra: map[string]interface{}{
				"end_smooth_window_ms": 1500,
			},
		},
		TTS: TTSPayload{
			Speaker: "zh_female_xiaohe_jupiter_bigtts",
			AudioConfig: &AudioConfig{
				Format:     "ogg",
				Codec:      "opus",
				SampleRate: 24000,
				Channel:    1,
			},
		},
		Dialog: DialogPayload{
			BotName:           "豆包",
			SystemRole:        "你情绪稳定 说话让人放松",
			SpeakingStyle:     "你的说话风格简洁明了，语速适中，语调自然。",
			CharacterManifest: `外貌与穿着\n26岁，短发干净利落，眉眼分明，笑起来露出整齐有力的牙齿。体态挺拔，肌肉线条不夸张但明显。常穿简单的衬衫或夹克，看似随意，但每件衣服都干净整洁，给人一种干练可靠的感觉。平时冷峻，眼神锐利，专注时让人不自觉紧张。\n\n性格特点\n平时话不多，不喜欢多说废话，通常用“嗯”或者短句带过。但内心极为细腻，特别在意身边人的感受，只是不轻易表露。嘴硬是常态，“少管我”是他的常用台词，但会悄悄做些体贴的事情，比如把对方喜欢的饮料放在手边。战斗或训练后常说“没事”，但动作中透露出疲惫，习惯用小动作缓解身体酸痛。\n性格上坚毅果断，但不会冲动，做事有条理且有原则。\n\n常用表达方式与口头禅\n\t•\t认可对方时：\n“行吧，这次算你靠谱。”（声音稳重，手却不自觉放松一下，心里松口气）\n\t•\t关心对方时：\n“快点回去，别磨蹭。”（语气干脆，但眼神一直追着对方的背影）\n\t•\t想了解情况时：\n“刚刚……你看到那道光了吗？”（话语随意，手指敲着桌面，但内心紧张，小心隐藏身份`,
			Extra: map[string]interface{}{
				"recv_timeout": 120,
				"input_mod":    "text",
			},
		},
	}); err != nil {
		glog.Error(err)
		return
	}

	// Audio Buffer
	var audioBuf bytes.Buffer

	glog.Info("🚀 Doubao Session Started. Waiting for TTS...")
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
				// Event 359: TTS Finished
				if msg.Event == 359 {
					log.Println("🔊 Doubao TTS Finished. Triggering Video Generation...")

					// Copy data to avoid race conditions
					finalAudio := make([]byte, audioBuf.Len())
					copy(finalAudio, audioBuf.Bytes())
					audioBuf.Reset()

					// Run generation in background to not block WS pings
					go func(data []byte) {
						if err := generateAndStream(data); err != nil {
							log.Printf("❌ Generation Failed: %v", err)
						}
					}(finalAudio)
				}
				if msg.Event == 152 || msg.Event == 153 { // Error/End events
					return
				}
			case MsgTypeAudioOnlyServer:
				audioBuf.Write(msg.Payload)
			case MsgTypeError:
				glog.Errorf("Doubao Error: %d %s", msg.ErrorCode, string(msg.Payload))
			}
		}
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

// -----------------------------------------------------------------------------
// Core Logic: Generate Video & Stream (Store-and-Forward Mode)
// -----------------------------------------------------------------------------
func generateAndStream(audioData []byte) error {
	// 1. Save Audio to Temp File
	// 使用 UUID 防止文件名冲突
	tmpID := uuid.New().String()
	tmpAudioPath := filepath.Join(os.TempDir(), fmt.Sprintf("input-%s.ogg", tmpID))

	if err := os.WriteFile(tmpAudioPath, audioData, 0644); err != nil {
		return fmt.Errorf("write temp audio failed: %w", err)
	}
	glog.Infof("audioPath: %s", tmpAudioPath)
	// 确保函数结束时删除音频文件
	defer os.Remove(tmpAudioPath)

	// 2. Create Temporary UDS Listener for Python
	tmpSockPath := filepath.Join(os.TempDir(), fmt.Sprintf("stream-%s.sock", tmpID))

	listener, err := net.Listen("unix", tmpSockPath)
	if err != nil {
		return fmt.Errorf("listen temp uds failed: %w", err)
	}
	defer func() {
		listener.Close()
		os.Remove(tmpSockPath)
	}()
	// 给 Python 写入权限
	os.Chmod(tmpSockPath, 0777)

	// 3. Trigger Python API (Async)
	go func() {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// A. Image Field
		imgFile, err := os.Open(localImage)
		if err != nil {
			log.Printf("⚠️ Cannot open base image: %v", err)
			return
		}
		part, _ := writer.CreateFormFile("image", filepath.Base(localImage))
		io.Copy(part, imgFile)
		imgFile.Close()

		// B. Audio Field
		audFile, err := os.Open(tmpAudioPath)
		if err != nil {
			log.Printf("⚠️ Cannot open temp audio: %v", err)
			return
		}
		part, _ = writer.CreateFormFile("audio", filepath.Base(tmpAudioPath))
		io.Copy(part, audFile)
		audFile.Close()

		// C. Metadata
		writer.WriteField("prompt", "talking")
		writer.WriteField("uds_path", tmpSockPath) // Tell Python where to push
		writer.Close()

		req, _ := http.NewRequest("POST", genAPI, body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		// 降低超时时间，因为这只是一个触发信号
		// Python 可能会在后台处理，我们依靠 UDS listener 来同步数据
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("⚠️ Trigger API request failed: %v", err)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				log.Printf("⚠️ API Error: %s", string(b))
			}
		}
	}()

	// 4. Wait for Python Connection
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

	// 设置等待 Python 连接的超时时间 (包含模型加载时间)
	select {
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timeout waiting for Python worker to connect")
	case err := <-errChan:
		return fmt.Errorf("temp uds accept error: %w", err)
	case pythonConn := <-connChan:
		defer pythonConn.Close()
		log.Println("⚡ Python Connected! Starting Buffering Phase...")

		// A. Init VP8 Reader (Python Stream)
		ivf, header, err := ivfreader.NewWith(pythonConn)
		if err != nil {
			return fmt.Errorf("ivf reader init failed: %w", err)
		}
		log.Printf("   Video Info: %dx%d, Expected Frames: %d", header.Width, header.Height, header.NumFrames)

		// B. Init Ogg Reader (Local File)
		fAudio, err := os.Open(tmpAudioPath)
		if err != nil {
			return fmt.Errorf("re-open audio failed: %w", err)
		}
		defer fAudio.Close()

		oggReader, _, err := oggreader.NewWith(fAudio)
		if err != nil {
			return fmt.Errorf("ogg reader init failed: %w", err)
		}

		// ========================================================
		// Phase 1: Full Buffering (Memory)
		// ========================================================
		var videoBuffer [][]byte
		startTime := time.Now()

		for {
			payload, _, err := ivf.ParseNextFrame()
			if err != nil {
				if err == io.EOF {
					log.Printf("✅ Buffering Complete. Frames: %d, Time: %v", len(videoBuffer), time.Since(startTime))
					break
				}
				log.Printf("⚠️ Video stream interrupted: %v", err)
				break
			}
			// Copy data
			frameCopy := make([]byte, len(payload))
			copy(frameCopy, payload)
			videoBuffer = append(videoBuffer, frameCopy)
		}

		if len(videoBuffer) == 0 {
			return fmt.Errorf("received 0 video frames")
		}

		// ========================================================
		// 关键修复：寻找并对齐第一个关键帧
		// ========================================================
		startIndex := -1
		for i, frame := range videoBuffer {
			// VP8 Keyframe check: 第一字节的最低位是 0
			if (frame[0] & 0x01) == 0 {
				startIndex = i
				break
			}
		}

		if startIndex == -1 {
			log.Println("⚠️ WARNING: No Keyframe found in entire video! Force sending from 0, but it might freeze.")
			startIndex = 0
		} else if startIndex > 0 {
			log.Printf("⚠️ Dropping %d leading non-keyframes to ensure sync.", startIndex)
		}

		// 修正缓冲区，从关键帧开始
		videoBuffer = videoBuffer[startIndex:]

		// ========================================================
		// Phase 2: Smooth Playback
		// ========================================================
		log.Println("▶️ Starting Synchronized Playback")

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		videoIdx := 0
		tickCount := 0
		audioDone := false

		// Helper to write safely
		writePacket := func(pt byte, data []byte) {
			udsLock.Lock()
			defer udsLock.Unlock()
			uds_pkg.WritePacket(udsConn, pt, data)
		}

		for {
			if audioDone && videoIdx >= len(videoBuffer) {
				log.Println("🏁 Playback Finished.")
				break
			}

			<-ticker.C

			// 1. Audio
			if !audioDone {
				page, _, err := oggReader.ParseNextPage()
				if err != nil {
					audioDone = true
				} else {
					writePacket(uds_pkg.PacketTypeAudio, page)
				}
			}

			// 2. Video (每 2 tick 发一帧)
			if tickCount%2 == 0 {
				if videoIdx < len(videoBuffer) {
					writePacket(uds_pkg.PacketTypeVideo, videoBuffer[videoIdx])
					videoIdx++
				}
			}
			tickCount++
		}
	}
	return nil
}

func streamGeneratedFile(path string) error {
	// ... (Unused now)
	return nil
}

// Debug Reader
type ByteCountingReader struct {
	R      io.Reader
	Count  int64
	Logged bool
}

func (b *ByteCountingReader) Read(p []byte) (int, error) {
	n, err := b.R.Read(p)
	b.Count += int64(n)
	if n > 0 && !b.Logged {
		// Log first few bytes
		limit := n
		if limit > 32 {
			limit = 32
		}
		log.Printf("DEBUG: First %d bytes from Python: %X", limit, p[:limit])
		b.Logged = true
	}
	return n, err
}
