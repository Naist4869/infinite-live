package uds

import (
	"infinite-live/internal/domain"
	"infinite-live/internal/infrastructure"
	"infinite-live/internal/pkg/protocol"
)

// ChannelSource adapts a packet channel (from Broadcaster) to FrameSource
type ChannelSource struct {
	ch                 <-chan *infrastructure.Packet
	waitingForKeyframe bool
}

func NewChannelSource(ch <-chan *infrastructure.Packet) *ChannelSource {
	return &ChannelSource{
		ch:                 ch,
		waitingForKeyframe: true,
	}
}

func (s *ChannelSource) Type() domain.AvatarState {
	return domain.StateTalking
}

func (s *ChannelSource) TryNextFrame() (*domain.MediaFrame, bool, error) {
	select {
	case pkt, ok := <-s.ch:
		if !ok {
			return nil, false, nil // Closed
		}
		return s.processPacket(pkt)
	default:
		return nil, false, nil
	}
}

func (s *ChannelSource) NextFrame() (*domain.MediaFrame, error) {
	for {
		pkt, ok := <-s.ch
		if !ok {
			return nil, nil
		}
		frame, hasData, err := s.processPacket(pkt)
		if err != nil {
			return nil, err
		}
		if hasData {
			return frame, nil
		}
	}
}

func (s *ChannelSource) processPacket(pkt *infrastructure.Packet) (*domain.MediaFrame, bool, error) {
	// 1. 设置正确的 Duration (纳秒级)
	// 音频 20ms, 视频 40ms (25fps)
	duration := 40
	if pkt.Type == protocol.PacketTypeAudio {
		duration = 20
	}

	// 2. 关键帧判断 (VP8 逻辑)
	isKey := false

	if pkt.Type == protocol.PacketTypeAudio {
		// 音频帧总是可以独立解码，视为关键帧
		isKey = true
	} else if pkt.Type == protocol.PacketTypeVideo && len(pkt.Payload) > 0 {
		// VP8 协议定义: 第一个字节的最低位(LSB)是 Keyframe flag
		// 0 = Key Frame
		// 1 = Inter Frame
		// 这一点与 H.264 完全不同，不要用查找 00 00 00 01 的方法
		isKey = (pkt.Payload[0] & 0x01) == 0
	}

	return &domain.MediaFrame{
		Data:     pkt.Payload,
		Duration: duration, // 修正单位
		IsKey:    isKey,    // 修正判断逻辑
		Type:     pkt.Type,
	}, true, nil
}

func (s *ChannelSource) Close() error {
	return nil
}
