package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// 配置常量
const (
	ApiKeyEnvVar     = "DASHSCOPE_API_KEY"
	CustomizationURL = "https://dashscope.aliyuncs.com/api/v1/services/audio/tts/customization"
	SynthesisURL     = "https://dashscope.aliyuncs.com/api/v1/services/audio/tts/synthesis"
	TargetModel      = "cosyvoice-v3-plus" // 推荐模型
)

// Client 封装 HTTP 请求
type Client struct {
	ApiKey string
}

// -------------------- 1. 创建音色 (Voice Enrollment) --------------------

type EnrollmentInput struct {
	Action        string   `json:"action"`
	TargetModel   string   `json:"target_model"`
	Prefix        string   `json:"prefix"`
	Url           string   `json:"url"`
	LanguageHints []string `json:"language_hints,omitempty"`
}

type EnrollmentRequest struct {
	Model string          `json:"model"`
	Input EnrollmentInput `json:"input"`
}

type EnrollmentResponse struct {
	Output struct {
		VoiceId string `json:"voice_id"`
	} `json:"output"`
	RequestId string `json:"request_id"`
}

func (c *Client) CreateVoice(prefix, audioUrl string) (string, error) {
	reqBody := EnrollmentRequest{
		Model: "voice-enrollment", // 固定值
		Input: EnrollmentInput{
			Action:        "create_voice",
			TargetModel:   TargetModel,
			Prefix:        prefix,
			Url:           audioUrl,
			LanguageHints: []string{"zh"}, // 默认为中文，v3-plus 模型支持
		},
	}

	respData, err := c.doRequest(CustomizationURL, reqBody)
	if err != nil {
		return "", err
	}

	var resp EnrollmentResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", fmt.Errorf("解析响应失败: %v", err)
	}

	if resp.Output.VoiceId == "" {
		return "", fmt.Errorf("创建失败，未返回 VoiceId，原始响应: %s", string(respData))
	}

	return resp.Output.VoiceId, nil
}

// -------------------- 2. 查询音色列表 (List Voices) --------------------

type ListVoiceInput struct {
	Action    string `json:"action"`
	Prefix    string `json:"prefix,omitempty"`
	PageIndex int    `json:"page_index"`
	PageSize  int    `json:"page_size"`
}

type ListVoiceRequest struct {
	Model string         `json:"model"`
	Input ListVoiceInput `json:"input"`
}

type VoiceInfo struct {
	VoiceId     string `json:"voice_id"`
	Status      string `json:"status"`
	GmtCreate   string `json:"gmt_create"`
	TargetModel string `json:"target_model"`
}

type ListVoiceResponse struct {
	Output struct {
		VoiceList []VoiceInfo `json:"voice_list"`
	} `json:"output"`
}

func (c *Client) ListVoices(prefix string) ([]VoiceInfo, error) {
	reqBody := ListVoiceRequest{
		Model: "voice-enrollment",
		Input: ListVoiceInput{
			Action:    "list_voice",
			Prefix:    prefix,
			PageIndex: 0,
			PageSize:  10,
		},
	}

	respData, err := c.doRequest(CustomizationURL, reqBody)
	if err != nil {
		return nil, err
	}

	var resp ListVoiceResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}

	return resp.Output.VoiceList, nil
}

// -------------------- 3. 语音合成 (Speech Synthesis) --------------------

type SynthesisInput struct {
	Text string `json:"text"`
}

type SynthesisParameters struct {
	TextType string `json:"text_type,omitempty"`
	Voice    string `json:"voice"`
	Format   string `json:"format,omitempty"`
}

type SynthesisRequest struct {
	Model      string              `json:"model"`
	Input      SynthesisInput      `json:"input"`
	Parameters SynthesisParameters `json:"parameters"`
}

// SynthesizeSpeech 发起合成请求并将音频写入文件
// 注意：RESTful 合成通常直接返回二进制音频流
func (c *Client) SynthesizeSpeech(text, voiceId, outputFile string) error {
	reqBody := SynthesisRequest{
		Model: TargetModel, // 必须与创建音色时的 target_model 一致
		Input: SynthesisInput{
			Text: text,
		},
		Parameters: SynthesisParameters{
			Voice:  voiceId,
			Format: "mp3",
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", SynthesisURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ApiKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("合成请求失败 Status: %d, Body: %s", resp.StatusCode, string(body))
	}

	// 检查 Content-Type，如果是 JSON 则说明可能出错了（虽然状态码是200，但阿里云有时会返回 JSON 错误）
	contentType := resp.Header.Get("Content-Type")
	if contentType == "application/json" {
		body, _ := io.ReadAll(resp.Body)
		// 简单的检查，如果 body 包含 "code" 或 "message"
		return fmt.Errorf("API 返回了 JSON 错误而非音频流: %s", string(body))
	}

	// 保存音频文件
	outFile, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

// -------------------- 通用 HTTP 请求工具 --------------------

func (c *Client) doRequest(url string, payload interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ApiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API 错误: %d, Body: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// -------------------- 主函数演示 --------------------

func main() {
	apiKey := os.Getenv(ApiKeyEnvVar)
	if apiKey == "" {
		fmt.Printf("请设置环境变量 %s\n", ApiKeyEnvVar)
		return
	}

	client := &Client{ApiKey: apiKey}
	voicePrefix := "mytest"

	// 替换为您自己的公网可访问音频 URL (必须是 WAV/MP3/M4A, 16k+, 单声道推荐)
	// 这里使用文档中的示例 URL
	audioUrl := "https://dashscope.oss-cn-beijing.aliyuncs.com/samples/audio/cosyvoice/cosyvoice-zeroshot-sample.wav"

	fmt.Println("=== 1. 开始创建音色 (Voice Enrollment) ===")
	voiceId, err := client.CreateVoice(voicePrefix, audioUrl)
	if err != nil {
		fmt.Printf("创建失败: %v\n", err)
		return
	}
	fmt.Printf("音色创建提交成功，VoiceID: %s\n", voiceId)

	fmt.Println("\n=== 等待音色训练完成 (轮询状态)... ===")
	// 音色复刻通常需要几秒钟，这里简单轮询
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		voices, err := client.ListVoices(voicePrefix)
		if err != nil {
			fmt.Printf("查询列表失败: %v\n", err)
			continue
		}

		found := false
		for _, v := range voices {
			if v.VoiceId == voiceId {
				found = true
				fmt.Printf("当前状态: %s\n", v.Status)
				if v.Status == "OK" {
					goto TrainingDone
				} else if v.Status == "UNDEPLOYED" {
					fmt.Println("音色训练失败，请检查音频质量。")
					return
				}
			}
		}
		if !found {
			fmt.Println("列表中未找到该 VoiceID...")
		}
	}
	fmt.Println("超时：音色未在规定时间内准备就绪。")
	return

TrainingDone:
	fmt.Println("音色已就绪！")

	fmt.Println("\n=== 2. 查询音色列表 (List Voices) ===")
	voices, err := client.ListVoices(voicePrefix)
	if err != nil {
		fmt.Printf("查询列表失败: %v\n", err)
	} else {
		for _, v := range voices {
			fmt.Printf("- ID: %s | 状态: %s | 模型: %s\n", v.VoiceId, v.Status, v.TargetModel)
		}
	}

	fmt.Println("\n=== 3. 语音合成 (Speech Synthesis) ===")
	outputFile := "output_audio.mp3"
	textToRead := "你好，这是使用 Go 语言通过阿里云 CosyVoice 复刻的声音生成的音频。"

	err = client.SynthesizeSpeech(textToRead, voiceId, outputFile)
	if err != nil {
		fmt.Printf("语音合成失败: %v\n", err)
		return
	}
	fmt.Printf("语音合成成功！音频已保存至: %s\n", outputFile)
}
