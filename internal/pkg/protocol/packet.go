package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	PacketTypeVideo     = 0x02 // 保持和 Python 习惯一致，或者强行统一为 1
	PacketTypeAudio     = 0x03
	PacketTypeText      = 0x01
	PacketTypeUserAudio = 0x04
)

// WritePacket writes a type-prefixed, length-prefixed packet
// Format: [Type:1][Length:4][Payload:N]
func WritePacket(w io.Writer, packetType byte, data []byte) error {
	length := uint32(len(data))

	// 🔥 优化：一次性分配内存，合并 Header 和 Payload
	// 这样只调用一次底层的 Write 系统调用，保证原子性，极大减少 UDS 碎片
	buf := make([]byte, 5+len(data))

	buf[0] = packetType
	binary.BigEndian.PutUint32(buf[1:], length)

	if len(data) > 0 {
		copy(buf[5:], data)
	}

	// Write entire packet at once
	if _, err := w.Write(buf); err != nil {
		return err
	}
	return nil
}

// ReadPacket reads a packet and returns type and data
func ReadPacket(r io.Reader) (byte, []byte, error) {
	// Read header (5 bytes)
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	length := binary.BigEndian.Uint32(header[1:])

	if length == 0 {
		return packetType, nil, io.EOF // Logic: 0 length packet = End of Stream
	} else if length > 10000000 { // Sanity check (10MB)
		return 0, nil, fmt.Errorf("packet too large: %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return packetType, data, nil
}
