package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/golang/glog"
	"github.com/gorilla/websocket"
)

type StartSessionPayload struct {
	ASR    ASRPayload    `json:"asr"`
	TTS    TTSPayload    `json:"tts,omitempty"` // Omit if using default Ogg/Opus?
	Dialog DialogPayload `json:"dialog"`
}

type ASRPayload struct {
	Extra map[string]interface{} `json:"extra"`
}

type TTSPayload struct {
	Speaker     string       `json:"speaker"`
	AudioConfig *AudioConfig `json:"audio_config,omitempty"`
}

type AudioConfig struct {
	Channel    int    `json:"channel"`
	Format     string `json:"format"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
}

type SayHelloPayload struct {
	Content string `json:"content"`
}

type ChatTTSTextPayload struct {
	Start   bool   `json:"start"`
	End     bool   `json:"end"`
	Content string `json:"content"`
}

type ChatRAGTextPayload struct {
	ExternalRAG string `json:"external_rag"`
}

type RAGObject struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

type DialogPayload struct {
	DialogID          string                 `json:"dialog_id"`
	BotName           string                 `json:"bot_name"`
	SystemRole        string                 `json:"system_role"`
	SpeakingStyle     string                 `json:"speaking_style"`
	CharacterManifest string                 `json:"character_manifest,omitempty"`
	Location          *LocationInfo          `json:"location,omitempty"`
	Extra             map[string]interface{} `json:"extra"`
}

type LocationInfo struct {
	Longitude   float64 `json:"longitude"`
	Latitude    float64 `json:"latitude"`
	City        string  `json:"city"`
	Country     string  `json:"country"`
	Province    string  `json:"province"`
	District    string  `json:"district"`
	Town        string  `json:"town"`
	CountryCode string  `json:"country_code"`
	Address     string  `json:"address"`
}

type ChatTextQueryPayload struct {
	Content string `json:"content"`
}

var (
	localSequence = atomic.Int64{}
	wsWriteLock   sync.Mutex
)

func startConnection(conn *websocket.Conn) error {
	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create StartSession request message: %w", err)
	}
	msg.Event = 1
	msg.Payload = []byte("{}")

	frame, err := protocol.Marshal(msg)
	glog.Infof("StartConnection frame: %v", frame)
	if err != nil {
		return fmt.Errorf("marshal StartConnection request message: %w", err)
	}

	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send StartConnection request: %w", err)
	}

	// Read ConnectionStarted message.
	mt, frame, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read ConnectionStarted response: %w", err)
	}
	if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
		return fmt.Errorf("unexpected Websocket message type: %d", mt)
	}

	msg, _, err = Unmarshal(frame, protocol.containsSequence)
	if err != nil {
		glog.Infof("StartConnection response: %s", frame)
		return fmt.Errorf("unmarshal ConnectionStarted response message: %w", err)
	}
	if msg.Type != MsgTypeFullServer {
		return fmt.Errorf("unexpected ConnectionStarted message type: %s", msg.Type)
	}
	if msg.Event != 50 {
		return fmt.Errorf("unexpected response event (%d) for StartConnection request", msg.Event)
	}
	glog.Infof("Connection started (event=%d) connectID: %s, payload: %s", msg.Event, msg.ConnectID, msg.Payload)

	return nil
}

func startSession(conn *websocket.Conn, sessionID string, req *StartSessionPayload) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal StartSession request payload: %w", err)
	}

	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create StartSession request message: %w", err)
	}
	msg.Event = 100
	msg.SessionID = sessionID
	msg.Payload = payload

	frame, err := protocol.Marshal(msg)
	glog.Infof("StartSession request frame: %v", frame)
	if err != nil {
		return fmt.Errorf("marshal StartSession request message: %w", err)
	}

	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send StartSession request: %w", err)
	}

	// Read SessionStarted message.
	mt, frame, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read SessionStarted response: %w", err)
	}
	if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
		return fmt.Errorf("unexpected Websocket message type: %d", mt)
	}

	// Validate SessionStarted message.
	msg, _, err = Unmarshal(frame, protocol.containsSequence)
	if err != nil {
		glog.Infof("StartSession response: %s", frame)
		return fmt.Errorf("unmarshal SessionStarted response message: %w", err)
	}
	if msg.Type != MsgTypeFullServer {
		return fmt.Errorf("unexpected SessionStarted message type: %s", msg.Type)
	}
	if msg.Event != 150 {
		return fmt.Errorf("unexpected response event (%d) for StartSession request", msg.Event)
	}
	glog.Infof("SessionStarted response payload: %v", string(msg.Payload))
	var jsonData map[string]interface{}
	if err := json.Unmarshal(msg.Payload, &jsonData); err != nil {
		return fmt.Errorf("unmarshal SessionStarted response payload: %w", err)
	}
	dialogID = jsonData["dialog_id"].(string)
	return nil
}

func sayHello(conn *websocket.Conn, sessionID string, req *SayHelloPayload) error {
	payload, err := json.Marshal(req)
	glog.Infof("SayHello request payload: %s", string(payload))
	if err != nil {
		return fmt.Errorf("marshal SayHello request payload: %w", err)
	}

	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create SayHello request message: %w", err)
	}
	msg.Event = 300
	msg.SessionID = sessionID
	msg.Payload = payload

	frame, err := protocol.Marshal(msg)
	glog.Infof("SayHello frame: %v", frame)
	if err != nil {
		return fmt.Errorf("marshal SayHello request message: %w", err)
	}

	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send SayHello request: %w", err)
	}
	return nil
}

func chatTextQuery(conn *websocket.Conn, sessionID string, req *ChatTextQueryPayload) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal ChatTextQuery request payload: %w", err)
	}
	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create ChatTextQuery request message: %w", err)
	}
	msg.Event = 501
	msg.SessionID = sessionID
	msg.Payload = payload
	frame, err := protocol.Marshal(msg)
	glog.Infof("ChatTextQuery request frame: %v", frame)
	if err != nil {
		return fmt.Errorf("marshal ChatTextQuery request message: %w", err)
	}
	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send ChatTextQuery request: %w", err)
	}
	return nil
}

// Removing chatTTSText and Audio Input functions as we just process Text Input -> Audio Output

func finishSession(conn *websocket.Conn, sessionID string) error {
	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create FinishSession request message: %w", err)
	}
	msg.Event = 102
	msg.SessionID = sessionID
	msg.Payload = []byte("{}")

	frame, err := protocol.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal FinishSession request message: %w", err)
	}

	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send FinishSession request: %w", err)
	}

	glog.Info("FinishSession request is sent.")
	return nil
}

func finishConnection(conn *websocket.Conn) error {
	msg, err := NewMessage(MsgTypeFullClient, MsgTypeFlagWithEvent)
	if err != nil {
		return fmt.Errorf("create FinishConnection request message: %w", err)
	}
	msg.Event = 2
	msg.Payload = []byte("{}")

	frame, err := protocol.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal FinishConnection request message: %w", err)
	}

	if err := sendRequest(conn, frame); err != nil {
		return fmt.Errorf("send FinishConnection request: %w", err)
	}

	// Read ConnectionStarted message.
	mt, frame, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read ConnectionFinished response: %w", err)
	}
	if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
		return fmt.Errorf("unexpected Websocket message type: %d", mt)
	}

	msg, _, err = Unmarshal(frame, protocol.containsSequence)
	if err != nil {
		glog.Infof("FinishConnection response: %s", frame)
		return fmt.Errorf("unmarshal ConnectionFinished response message: %w", err)
	}
	if msg.Type != MsgTypeFullServer {
		return fmt.Errorf("unexpected ConnectionFinished message type: %s", msg.Type)
	}
	if msg.Event != 52 {
		return fmt.Errorf("unexpected response event (%d) for FinishConnection request", msg.Event)
	}

	glog.Infof("Connection finished (event=%d).", msg.Event)
	return nil
}

func sendRequest(conn *websocket.Conn, frame []byte) error {
	wsWriteLock.Lock()
	defer wsWriteLock.Unlock()
	if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		// return fmt.Errorf("send SayHello request: %w", err) // Original err msg was wrong context
		return fmt.Errorf("sendMessage error: %w", err)
	}
	localSequence.Add(1)
	return nil
}
