package file

import (
	"errors"
	"infinite-live/internal/domain"
	"infinite-live/internal/pkg/protocol"
	"io"
	"os"
	"sync"

	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

// LoopReader for VP8 (IVF container)
type LoopReader struct {
	filePath  string
	stateType domain.AvatarState
	file      *os.File
	ivf       *ivfreader.IVFReader
	header    *ivfreader.IVFFileHeader
	loop      bool
	mu        sync.Mutex
}

func NewLoopReader(path string, state domain.AvatarState) (*LoopReader, error) {
	return newReader(path, state, true)
}

func NewSequentialReader(path string, state domain.AvatarState) (*LoopReader, error) {
	return newReader(path, state, false)
}

func newReader(path string, state domain.AvatarState, loop bool) (*LoopReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	reader, header, err := ivfreader.NewWith(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &LoopReader{
		filePath:  path,
		stateType: state,
		file:      f,
		ivf:       reader,
		header:    header,
		loop:      loop,
	}, nil
}

func (r *LoopReader) Type() domain.AvatarState {
	return r.stateType
}

func (r *LoopReader) NextFrame() (*domain.MediaFrame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ivf == nil {
		return nil, errors.New("reader closed")
	}

	payload, _, err := r.ivf.ParseNextFrame()
	if err != nil {
		if err == io.EOF {
			if !r.loop {
				return nil, io.EOF
			}
			// Loop logic: Rewind
			if _, seekErr := r.file.Seek(0, 0); seekErr != nil {
				return nil, seekErr
			}
			// Re-init reader (skips file header automatically)
			newReader, _, err := ivfreader.NewWith(r.file)
			if err != nil {
				return nil, err
			}
			r.ivf = newReader

			// Retry reading first frame
			payload, _, err = r.ivf.ParseNextFrame()
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// VP8 Keyframe detection:
	// A keyframe is defined by the P bit (inverse) in the first byte of payload.
	// Actually for VP8 in IVF:
	// The least significant bit of the first byte is 0 for key frames.
	isKey := (payload[0] & 0x01) == 0

	return &domain.MediaFrame{
		Type:     protocol.PacketTypeVideo, // <--- 【关键修复】加上这一行！
		Data:     payload,
		Duration: 40, // 25fps fixed
		IsKey:    isKey,
	}, nil
}

func (r *LoopReader) TryNextFrame() (*domain.MediaFrame, bool, error) {
	frame, err := r.NextFrame()
	return frame, true, err
}

func (r *LoopReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
func (r *LoopReader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. 文件指针回到开头
	if _, err := r.file.Seek(0, 0); err != nil {
		return err
	}

	// 2. 重新初始化 IVF Reader (这一步很重要，因为要跳过 IVF 文件头)
	newIvf, _, err := ivfreader.NewWith(r.file)
	if err != nil {
		return err
	}
	r.ivf = newIvf

	return nil
}
