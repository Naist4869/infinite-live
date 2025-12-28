package domain

import (
	"errors"
)

// AvatarState represents the current state of the digital human
type AvatarState int

const (
	StateIdle AvatarState = iota
	StateListening
	StateThinking
	StateTalking
)

func (s AvatarState) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateListening:
		return "Listening"
	case StateThinking:
		return "Thinking"
	case StateTalking:
		return "Talking"
	default:
		return "Unknown"
	}
}

// MediaFrame represents a single frame of Data (Video or Audio)
type MediaFrame struct {
	Data     []byte
	Duration int
	IsKey    bool
	Type     byte // 1=Video, 2=Audio. See protocol package.
}

// FrameSource is an interface for getting video/audio frames
// This could be a local file looper or a live stream from Python
type FrameSource interface {
	NextFrame() (*MediaFrame, error)
	TryNextFrame() (*MediaFrame, bool, error) // Returns frame, hasData, error
	Type() AvatarState
	Close() error
}

// Resetter 定义重置能力
type Resetter interface {
	Reset() error
}

// ResettableFrameSource 是通过“组装”得到的接口
// 只有 Idle 资源需要实现这个接口
type ResettableFrameSource interface {
	FrameSource
	Resetter
}

// StreamPublisher is an interface for publishing frames to WebRTC
type StreamPublisher interface {
	Publish(frame *MediaFrame) error
}

// AIGenerator is an interface for the AI generation service
type AIGenerator interface {
	Generate(text string) (streamID string, err error)
}

var (
	ErrStreamEnded = errors.New("stream ended")
)
