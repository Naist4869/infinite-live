package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// ================= 配置区域 =================
const (
	// 请确保环境变量 DASHSCOPE_API_KEY 已设置
	BaseURL = "https://dashscope-intl.aliyuncs.com/api/v1/services/audio/tts/customization"
	WsURL   = "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime"
	//BaseURL         = "https://dashscope.aliyuncs.com/api/v1/services/audio/tts/customization"
	//WsURL           = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	EnrollmentModel = "qwen-voice-enrollment"
	TargetModel     = "qwen3-tts-vc-realtime-2026-01-15"             // 必须与合成时一致
	VoiceFilePath   = "/home/lan/src/infinite-live/assets/开心元元7.MP3" // 本地录音文件路径
	OutputPcmPath   = "output.pcm"                                   // 合成结果保存路径
	AUDIO_MIME_TYPE = "audio/mpeg"
)

// ================= 结构体定义 =================

// EnrollmentRequest 用于创建和查询音色
type EnrollmentRequest struct {
	Model string                 `json:"model"`
	Input EnrollmentRequestInput `json:"input"`
}

type EnrollmentRequestInput struct {
	Action        string     `json:"action"`
	TargetModel   string     `json:"target_model,omitempty"`
	PreferredName string     `json:"preferred_name,omitempty"`
	PageSize      int        `json:"page_size,omitempty"`
	PageIndex     int        `json:"page_index,omitempty"`
	Audio         *AudioData `json:"audio,omitempty"`
}

type AudioData struct {
	Data string `json:"data"`
}

// EnrollmentResponse API 响应结构
type EnrollmentResponse struct {
	Output    EnrollmentOutput `json:"output"`
	RequestID string           `json:"request_id"`
}

type EnrollmentOutput struct {
	Voice     string      `json:"voice"`      // 创建成功返回的音色ID
	VoiceList []VoiceItem `json:"voice_list"` // 查询列表返回的项
}

type VoiceItem struct {
	Voice       string `json:"voice"`
	GmtCreate   string `json:"gmt_create"`
	TargetModel string `json:"target_model"`
}

// RealtimeMessage WebSocket 消息通用结构
type RealtimeMessage struct {
	Type    string            `json:"type"`
	EventID string            `json:"event_id,omitempty"`
	Session *SessionConfig    `json:"session,omitempty"`
	Item    *ConversationItem `json:"item,omitempty"`
	Delta   string            `json:"delta,omitempty"` // 音频 Base64 数据
}

type SessionConfig struct {
	Voice             string `json:"voice,omitempty"`
	Model             string `json:"model,omitempty"`
	InputAudioFormat  string `json:"input_audio_format,omitempty"`
	OutputAudioFormat string `json:"output_audio_format,omitempty"`
}

type ConversationItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ================= 主逻辑 =================

func main() {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		log.Fatal("请设置环境变量 DASHSCOPE_API_KEY")
	}

	var voiceId string
	// 1. 创建音色
	fmt.Println("--- 1. 开始创建音色 ---")
	voiceID, err := createVoice(apiKey, VoiceFilePath)
	if err != nil {
		log.Fatalf("创建音色失败: %v", err)
	}
	fmt.Printf("音色创建成功，Voice ID: %s\n", voiceID)

	// 2. 查询音色列表
	fmt.Println("\n--- 2. 查询音色列表 ---")
	list, err := listVoices(apiKey)
	if err != nil {
		log.Fatalf("查询列表失败: %v", err)
	}

	if len(list) == 0 {
		log.Fatalf("无可用voiceId")
	}
	if voiceId == "" {
		voiceId = list[0].Voice
	}

	// 3. 语音合成
	fmt.Println("\n--- 3. 开始语音合成 ---")
	textToSynthesize := "尤其是过年的时候，去逛超市，就会觉得超级超级开心！"
	err = synthesizeVoice(apiKey, voiceID, textToSynthesize)
	if err != nil {
		log.Fatalf("语音合成失败: %v", err)
	}
	fmt.Printf("语音合成完成，文件已保存至: %s\n", OutputPcmPath)
	fmt.Println("提示: 可以使用 ffplay 播放: ffplay -f s16le -ar 24000 -ac 1 output.pcm")
}

// ================= 功能实现 =================

// createVoice 创建音色
func createVoice(apiKey, filePath string) (string, error) {
	// 读取并 Base64 编码音频文件
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("读取音频文件失败: %v", err)
	}
	base64Audio := base64.StdEncoding.EncodeToString(fileBytes)
	// 假设是 mp3，如果是 wav 需改为 audio/wav
	dataURI := fmt.Sprintf("data:%s;base64,%s", AUDIO_MIME_TYPE, base64Audio)

	reqBody := EnrollmentRequest{
		Model: EnrollmentModel,
		Input: EnrollmentRequestInput{
			Action:        "create",
			TargetModel:   TargetModel,
			PreferredName: "yuanyuan7mp3",
			Audio:         &AudioData{Data: dataURI},
		},
	}

	respData, err := postRequest(apiKey, reqBody)
	if err != nil {
		return "", err
	}

	return respData.Output.Voice, nil
}

// listVoices 查询音色列表
func listVoices(apiKey string) ([]VoiceItem, error) {
	reqBody := EnrollmentRequest{
		Model: EnrollmentModel,
		Input: EnrollmentRequestInput{
			Action:    "list",
			PageSize:  5,
			PageIndex: 0,
		},
	}

	respData, err := postRequest(apiKey, reqBody)
	if err != nil {
		return nil, err
	}

	for i, v := range respData.Output.VoiceList {
		fmt.Printf("[%d] ID: %s | 创建时间: %s | 模型: %s\n", i+1, v.Voice, v.GmtCreate, v.TargetModel)
	}
	return respData.Output.VoiceList, nil
}

// synthesizeVoice 实时语音合成 (WebSocket) - 修正版
func synthesizeVoice(apiKey, voiceID, text string) error {
	// 1. 建立 WebSocket 连接
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)

	// 必须在 URL 中显式指定 model 参数
	urlWithModel := fmt.Sprintf("%s?model=%s", WsURL, TargetModel)
	log.Printf("连接 WebSocket: %s", urlWithModel)

	conn, _, err := websocket.DefaultDialer.Dial(urlWithModel, headers)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %v", err)
	}
	defer conn.Close()

	// 打开文件准备写入 PCM 数据
	outFile, err := os.Create(OutputPcmPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	done := make(chan struct{})

	// 2. 接收消息的协程
	go func() {
		defer close(done)
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure) {
					log.Printf("读取错误: %v", err)
				}
				return
			}

			// 使用 map 解析通用消息，避免结构体定义遗漏
			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				continue
			}

			msgType, _ := msg["type"].(string)

			switch msgType {
			case "session.created":
				fmt.Println("-> 会话已建立")
			case "response.audio.delta":
				// 获取 base64 音频数据
				if delta, ok := msg["delta"].(string); ok && delta != "" {
					audioBytes, _ := base64.StdEncoding.DecodeString(delta)
					outFile.Write(audioBytes)
					fmt.Print(".") // 进度条
				}
			case "response.done":
				fmt.Println("\n-> 生成完成")
			case "session.finished":
				fmt.Println("\n-> 会话结束")
				return
			case "error":
				// 打印详细错误
				fmt.Printf("\n[Server Error]: %v\n", msg)
			}
		}
	}()

	// 3. 发送指令流程 (TTS 专用流程)

	// 步骤 A: session.update (配置音色)
	// 使用 map[string]interface{} 构造 JSON 以确保字段准确
	err = conn.WriteJSON(map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"voice":               voiceID,
			"model":               TargetModel,
			"input_audio_format":  "pcm",
			"output_audio_format": "pcm",
		},
	})
	if err != nil {
		return err
	}

	// 步骤 B: input_text_buffer.append (发送文本)
	// 注意：这里使用的是 text 字段，而不是 conversation item
	err = conn.WriteJSON(map[string]interface{}{
		"type": "input_text_buffer.append",
		"text": text,
	})
	if err != nil {
		return err
	}

	// 步骤 C: input_text_buffer.commit (提交任务)
	// 告诉服务器文本发送完毕，开始合成
	err = conn.WriteJSON(map[string]interface{}{
		"type": "input_text_buffer.commit",
	})
	if err != nil {
		return err
	}

	// 4. 等待完成
	select {
	case <-done:
		// 正常结束
	case <-time.After(30 * time.Second): // 增加超时时间防止长文本合成中断
		fmt.Println("\n超时，强制关闭连接")
	}

	return nil
}

// postRequest 通用的 HTTP POST 辅助函数
func postRequest(apiKey string, body interface{}) (*EnrollmentResponse, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", BaseURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBytes))
	}

	var result EnrollmentResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
