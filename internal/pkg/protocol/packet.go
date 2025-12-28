package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	PacketTypeVideo     = 0x01
	PacketTypeAudio     = 0x02
	PacketTypeText      = 0x03
	PacketTypeUserAudio = 0x04 // <--- 新增这个：代表用户说话的音频

)

// WritePacket writes a type-prefixed, length-prefixed packet
// Format: [Type:1][Length:4][Payload:N]
func WritePacket(w io.Writer, packetType byte, data []byte) error {
	length := uint32(len(data))
	header := make([]byte, 5) // 1 byte type + 4 bytes length
	header[0] = packetType
	binary.BigEndian.PutUint32(header[1:], length)

	// Write header
	if _, err := w.Write(header); err != nil {
		return err
	}
	// Write payload
	if _, err := w.Write(data); err != nil {
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
