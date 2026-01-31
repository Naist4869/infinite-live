package file

import (
	"infinite-live/internal/infrastructure"
	"infinite-live/internal/pkg/protocol"
)

type UDSWriter struct {
	Broadcaster *infrastructure.UDSBroadcaster
}

func (w *UDSWriter) Write(p []byte) (n int, err error) {
	if w.Broadcaster != nil {
		// 这里必须拷贝一份数据，因为 p 在调用结束后可能会被复用
		payload := make([]byte, len(p))
		copy(payload, p)

		// ⚠️ 注意：这里发送的是封装好的 Ogg 数据块
		// 对应 Python 端需要用 IsPCM=false 来接收
		w.Broadcaster.SendToWorker(protocol.PacketTypeUserAudio, payload)
	}
	return len(p), nil
}
