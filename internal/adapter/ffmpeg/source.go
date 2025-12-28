package ffmpeg

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"infinite-live/internal/domain"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

type StreamSource struct {
	cmd        *exec.Cmd
	stdout     io.ReadCloser
	h264Reader *h264reader.H264Reader
	nalChan    chan *h264reader.NAL
}

func NewStreamSource(filePath string, loop bool, copyVideo bool) (*StreamSource, error) {
	absPath, _ := filepath.Abs(filePath)

	loopArg := []string{}
	if loop {
		loopArg = []string{"-stream_loop", "-1"}
	}

	// FFmpeg command construction
	args := []string{}
	args = append(args, loopArg...)
	args = append(args, "-i", absPath)

	transcodeArgs := []string{}

	if copyVideo {
		// Just copy the video bitstream (Efficient!)
		// Ensure output format is h264
		transcodeArgs = []string{
			"-c:v", "copy",
			"-bsf:v", "h264_mp4toannexb", // Essential for MP4 -> Annex B
			"-f", "h264",
			"pipe:1",
		}
	} else {
		// Transcoding Options (High Res, High Speed)
		transcodeArgs = []string{
			"-c:v", "libx264",
			"-preset", "ultrafast", // Max speed
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-s", "512x768", // Native resolution
			"-r", "25",
			"-pix_fmt", "yuv420p",
			"-g", "50",
			"-keyint_min", "50",
			"-sc_threshold", "0",
			"-bsf:v", "h264_mp4toannexb",
			"-f", "h264",
			"pipe:1",
		}
	}
	args = append(args, transcodeArgs...)

	cmd := exec.Command("ffmpeg", args...)

	// Hook up Stderr for debugging
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Create h264 reader from the stdout stream
	// Note: We use a buffer to ensure smooth reading
	reader, err := h264reader.NewReader(bufio.NewReader(stdout))
	if err != nil {
		return nil, err
	}

	return &StreamSource{
		cmd:        cmd,
		stdout:     stdout,
		h264Reader: reader,
	}, nil
}

func (s *StreamSource) Type() domain.AvatarState {
	return domain.StateTalking // Or whatever
}

func (s *StreamSource) NextFrame() (*domain.MediaFrame, error) {
	// Read next NAL
	nal, err := s.h264Reader.NextNAL()
	if err != nil {
		if err != io.EOF {
			// DO NOT import log here, just return err
			// Actually let's import log if needed, but file doesn't have it.
			// Let's just return err. The caller logs it.
		}
		return nil, err
	}
	// Debug: Print first 5 bytes of data
	// log.Printf("NAL: %d bytes", len(nal.Data))

	// Convert to bytes
	// NAL struct has Data []byte
	// We need to prepend start code?
	// Pion h264reader returns the NAL unit content.
	// H.264 stream via WebRTC typically wants NAL units.
	// But our simple UDS parser in `file.go` or `uds_source.go` might expect something specific?
	// actually `pion` WriteSample expects the NAL unit without start code typically, OR with it?
	// Pion's sample: "The data in the sample is expected to be a valid H264 NAL unit."

	// Append Start Code (Annex B) - Standard 4 bytes.
	// This matches file/source.go logic which is proven to work.
	// Pion needs this or the browser needs this to delineate NALs.
	fullData := append([]byte{0, 0, 0, 1}, nal.Data...)

	return &domain.MediaFrame{
		Data:     fullData, // Now includes Start Code
		Duration: 40,       // Mock duration
		IsKey:    false,    // We could parse nal.UnitType to check IDR
	}, nil
}

func (s *StreamSource) TryNextFrame() (*domain.MediaFrame, bool, error) {
	// For FFmpeg stream, we treat it as always ready (blocking read)
	frame, err := s.NextFrame()
	if err != nil {
		return nil, false, err
	}
	return frame, true, nil
}

func (s *StreamSource) Close() error {
	if s.cmd.Process != nil {
		return s.cmd.Process.Kill()
	}
	return nil
}
