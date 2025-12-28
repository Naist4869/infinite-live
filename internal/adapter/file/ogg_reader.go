package file

import (
	"errors"
	"infinite-live/internal/domain"
	"infinite-live/internal/pkg/protocol"
	"io"
	"os"
	"sync"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

type OggLoopReader struct {
	filePath  string
	stateType domain.AvatarState // 新增：保存状态类型
	file      *os.File
	ogg       *oggreader.OggReader
	mu        sync.Mutex
}

// NewOggLoopReader 默认将音频状态设为 Idle
func NewOggLoopReader(path string) (*OggLoopReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	ogg, _, err := oggreader.NewWith(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &OggLoopReader{
		filePath:  path,
		stateType: domain.StateIdle, // 默认为 Idle 状态
		file:      f,
		ogg:       ogg,
	}, nil
}

// Type 实现 FrameSource 接口
func (r *OggLoopReader) Type() domain.AvatarState {
	return r.stateType
}

// TryNextFrame 实现 FrameSource 接口
// 对于本地文件，我们认为它总是“准备好”的，所以直接调用 NextFrame
func (r *OggLoopReader) TryNextFrame() (*domain.MediaFrame, bool, error) {
	frame, err := r.NextFrame()
	if err != nil {
		return nil, false, err
	}
	return frame, true, nil
}

// NextFrame 实现 FrameSource 接口
func (r *OggLoopReader) NextFrame() (*domain.MediaFrame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ogg == nil {
		return nil, errors.New("ogg reader closed")
	}

	data, _, err := r.ogg.ParseNextPage()
	if err != nil {
		if err == io.EOF {
			// 循环逻辑：倒带
			if _, seekErr := r.file.Seek(0, 0); seekErr != nil {
				return nil, seekErr
			}

			// 重新初始化 Reader
			newOgg, _, newErr := oggreader.NewWith(r.file)
			if newErr != nil {
				return nil, newErr
			}
			r.ogg = newOgg

			// 重试读取
			data, _, err = r.ogg.ParseNextPage()
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	return &domain.MediaFrame{
		Type:     protocol.PacketTypeAudio,
		Data:     data,
		Duration: 20,
		IsKey:    true,
	}, nil
}

func (r *OggLoopReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

func (r *OggLoopReader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Seek(0, 0); err != nil {
		return err
	}
	newOgg, _, err := oggreader.NewWith(r.file)
	if err != nil {
		return err
	}
	r.ogg = newOgg
	return nil
}
