package livekit

import (
	"infinite-live/internal/domain"
	"infinite-live/internal/pkg/protocol"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4/pkg/media"
)

type LiveKitPublisher struct {
	videoTrack *lksdk.LocalSampleTrack
	audioTrack *lksdk.LocalSampleTrack
}

func NewLiveKitPublisher(video *lksdk.LocalSampleTrack, audio *lksdk.LocalSampleTrack) *LiveKitPublisher {
	return &LiveKitPublisher{
		videoTrack: video,
		audioTrack: audio,
	}
}

func (p *LiveKitPublisher) Publish(frame *domain.MediaFrame) error {
	sample := media.Sample{
		Data:     frame.Data,
		Duration: 40 * time.Millisecond, // 强制固定帧率 25fps，VP8 建议这样写死
	}

	if frame.Type == protocol.PacketTypeVideo {
		// 直接写入 VP8 数据，Pion 会自动根据 Track 的 MimeType 进行 RTP 打包
		return p.videoTrack.WriteSample(sample, nil)
	} else if frame.Type == protocol.PacketTypeAudio {
		sample.Duration = 20 * time.Millisecond
		return p.audioTrack.WriteSample(sample, nil)
	}
	return nil
}
