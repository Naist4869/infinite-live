package uds

import (
	"fmt"
	"infinite-live/internal/domain"
	"infinite-live/internal/infrastructure"
	"infinite-live/internal/pkg/protocol"
	"io"
	"log"
	"net"
	"sync"
)

// UDSReceiverSource adapts a UDS connection to a FrameSource
type UDSReceiverSource struct {
	server *infrastructure.UDSServer
	conn   net.Conn
	mu     sync.Mutex
	connCh chan net.Conn
}

func NewUDSReceiverSource(server *infrastructure.UDSServer) *UDSReceiverSource {
	return &UDSReceiverSource{
		server: server,
		connCh: make(chan net.Conn, 1),
	}
}

// StartAccepting runs a background loop to accept connections
func (u *UDSReceiverSource) Start() {
	go func() {
		for {
			conn, err := u.server.Accept()
			if err != nil {
				log.Printf("UDS Accept Error: %v", err)
				return
			}
			log.Println("UDS Source: Accepted new worker connection")

			u.mu.Lock()
			if u.conn != nil {
				u.conn.Close()
			}
			u.conn = conn
			u.mu.Unlock()

			// 同时尝试推入通道，以防 NextFrame 正在阻塞等待
			select {
			case u.connCh <- conn:
			default:
			}
		}
	}()
}

func (u *UDSReceiverSource) Type() domain.AvatarState {
	return domain.StateTalking
}

func (u *UDSReceiverSource) NextFrame() (*domain.MediaFrame, error) {
	u.mu.Lock()
	conn := u.conn
	u.mu.Unlock()

	if conn == nil {
		select {
		case newConn := <-u.connCh:
			u.mu.Lock()
			u.conn = newConn
			conn = newConn
			u.mu.Unlock()
		default:
			return nil, fmt.Errorf("no worker connected")
		}
	}

	pktType, payload, err := protocol.ReadPacket(conn)
	if err != nil {
		if err == io.EOF {
			u.mu.Lock()
			// 只有当 conn 没变时才清理，防止清理了新连接
			if u.conn == conn {
				u.conn.Close()
				u.conn = nil
			}
			u.mu.Unlock()
		}
		return nil, err
	}

	// 1. 时间单位修正
	duration := 40
	if pktType == protocol.PacketTypeAudio {
		duration = 20
	}

	// 2. VP8 关键帧判断
	isKey := false
	if pktType == protocol.PacketTypeAudio {
		isKey = true
	} else if pktType == protocol.PacketTypeVideo && len(payload) > 0 {
		isKey = (payload[0] & 0x01) == 0
	}

	return &domain.MediaFrame{
		Data:     payload,
		Duration: duration,
		IsKey:    isKey,
		Type:     pktType,
	}, nil
}

func (u *UDSReceiverSource) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn != nil {
		return u.conn.Close()
	}
	return nil
}
