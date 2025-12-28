package usecase

import (
	"log"
	"time"

	"infinite-live/internal/domain"
	"infinite-live/internal/pkg/protocol"
)

type LiveInteractor struct {
	publisher domain.StreamPublisher

	idleVideoSource domain.ResettableFrameSource
	idleAudioSource domain.ResettableFrameSource
	talkingSource   domain.FrameSource

	// 缓冲区加大到 1000，防止长时间视频导致通道阻塞死锁
	talkingVideoCh chan *domain.MediaFrame
	talkingAudioCh chan *domain.MediaFrame

	currentState domain.AvatarState
	stopChan     chan struct{}
}

func NewLiveInteractor(
	pub domain.StreamPublisher,
	idleVideo domain.ResettableFrameSource,
	idleAudio domain.ResettableFrameSource,
) *LiveInteractor {
	return &LiveInteractor{
		publisher:       pub,
		idleVideoSource: idleVideo,
		idleAudioSource: idleAudio,
		// 修改点：加大缓冲区
		talkingVideoCh: make(chan *domain.MediaFrame, 1000),
		talkingAudioCh: make(chan *domain.MediaFrame, 1000),
		currentState:   domain.StateIdle,
		stopChan:       make(chan struct{}),
	}
}

func (l *LiveInteractor) StartLoop() {
	time.Sleep(1 * time.Second)
	log.Println("LiveInteractor: Starting Loops...")
	go l.routeTalkingData()
	go l.runAudioLoop()
	l.runVideoLoop()
}

// 数据分流器
func (l *LiveInteractor) routeTalkingData() {
	log.Println("Router: Started...")
	for {
		select {
		case <-l.stopChan:
			return
		default:
			if l.talkingSource == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			frame, hasData, err := l.talkingSource.TryNextFrame()
			if err != nil || !hasData || frame == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}

			// 阻塞写入，确保不丢包
			if frame.Type == protocol.PacketTypeVideo {
				select {
				case l.talkingVideoCh <- frame:
				case <-l.stopChan:
					return
				}
			} else if frame.Type == protocol.PacketTypeAudio {
				select {
				case l.talkingAudioCh <- frame:
				case <-l.stopChan:
					return
				}
			}
		}
	}
}

// 音频循环
func (l *LiveInteractor) runAudioLoop() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopChan:
			return
		case <-ticker.C:
			// 优先播放 Talking 音频
			select {
			case talkFrame := <-l.talkingAudioCh:
				l.publisher.Publish(talkFrame)
				continue
			default:
			}

			// 其次播放 Idle 音频
			if l.idleAudioSource != nil {
				frame, err := l.idleAudioSource.NextFrame()
				if err == nil {
					l.publisher.Publish(frame)
				}
			}
		}
	}
}

// 视频循环
func (l *LiveInteractor) runVideoLoop() {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()

	waitingForKeyframe := true
	lastState := domain.StateIdle
	lastTalkTime := time.Now().Add(-10 * time.Hour)

	for {
		select {
		case <-l.stopChan:
			return
		case <-ticker.C:
			select {
			case talkFrame := <-l.talkingVideoCh:
				lastTalkTime = time.Now()

				// 检测状态切换
				if lastState == domain.StateIdle {
					waitingForKeyframe = true
				}

				lastState = domain.StateTalking
				l.currentState = domain.StateTalking

				// 关键帧检测
				if waitingForKeyframe {
					if !talkFrame.IsKey {
						// 如果这里打印了日志，说明 main.go 的关键帧过滤没生效
						// 或者 VP8 数据流有问题
						// log.Println("⚠️ Skipped P-Frame, waiting for Keyframe...")
						continue
					}
					log.Println("✅ Talking Started (Keyframe Rendered)")
					waitingForKeyframe = false
				}

				l.publisher.Publish(talkFrame)
				continue

			default:
			}

			// Anti-Flicker: 500ms 保护期
			if time.Since(lastTalkTime) < 80*time.Millisecond {
				continue
			}

			if lastState == domain.StateTalking {
				log.Println("Talking finished. Switching back to Idle.")
				lastState = domain.StateIdle
				l.currentState = domain.StateIdle
				waitingForKeyframe = true

				// 只重置视频！
				if err := l.idleVideoSource.Reset(); err != nil {
					log.Printf("❌ Failed to reset idle video: %v", err)
				}

			}

			frame, err := l.idleVideoSource.NextFrame()
			if err == nil && frame != nil {
				l.publisher.Publish(frame)
			}
		}
	}
}

func (l *LiveInteractor) OnUserComment(text string) {
	log.Printf("Interactor received: %s", text)
}

func (l *LiveInteractor) SetTalkingSource(s domain.FrameSource) {
	l.talkingSource = s
}

func (l *LiveInteractor) Stop() {
	close(l.stopChan)
}
