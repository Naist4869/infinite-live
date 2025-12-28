package webrtc

import (
	"fmt"
	"time"

	"infinite-live/internal/domain"
	"infinite-live/internal/pkg/protocol"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PionPublisher struct {
	videoTrack *webrtc.TrackLocalStaticSample
	audioTrack *webrtc.TrackLocalStaticSample
}

func NewPionPublisher(video *webrtc.TrackLocalStaticSample, audio *webrtc.TrackLocalStaticSample) *PionPublisher {
	return &PionPublisher{
		videoTrack: video,
		audioTrack: audio,
	}
}

func (p *PionPublisher) Publish(frame *domain.MediaFrame) error {
	if frame.Type == protocol.PacketTypeVideo {
		// Video Logic
		// frame.Data now contains full AU (SPS+PPS+IDR etc), so one timestamp increment is correct.
		return p.videoTrack.WriteSample(media.Sample{
			Data:     frame.Data,
			Duration: time.Duration(frame.Duration) * time.Millisecond,
		})
	} else if frame.Type == protocol.PacketTypeAudio {
		// Audio Logic (Opus)
		return p.audioTrack.WriteSample(media.Sample{
			Data:     frame.Data,
			Duration: time.Duration(20) * time.Millisecond,
		})
	}
	return nil
}

// Factory to create track
func NewVideoTrack() (*webrtc.TrackLocalStaticSample, error) {
	// Create a video track
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"pion-webrtc",
	)
	if err != nil {
		return nil, fmt.Errorf("error creating track: %w", err)
	}
	return videoTrack, nil
}
