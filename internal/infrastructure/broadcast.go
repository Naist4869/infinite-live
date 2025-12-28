package infrastructure

import (
	"infinite-live/internal/pkg/protocol"
	"io"
	"log"
	"net"
	"sync"
)

// UDSBroadcaster accepts a connection from Mock AI and broadcasts packets to all listeners.
type UDSBroadcaster struct {
	server    *UDSServer
	listeners map[chan *Packet]struct{}
	activeConn net.Conn
	mu        sync.RWMutex
	stopCh    chan struct{}
}

type Packet struct {
	Type    byte
	Payload []byte
}

func NewUDSBroadcaster(server *UDSServer) *UDSBroadcaster {
	return &UDSBroadcaster{
		server:    server,
		listeners: make(map[chan *Packet]struct{}),
		stopCh:    make(chan struct{}),
	}
}

func (b *UDSBroadcaster) Start() {
	go func() {
		for {
			// Accept ONE connection from Mock AI
			conn, err := b.server.Accept()
			if err != nil {
				log.Printf("Broadcaster Accept Error: %v", err)
				return
			}
			log.Println("Broadcaster: Mock AI Connected. Broadcasting...")

			b.mu.Lock()
			b.activeConn = conn
			b.mu.Unlock()

			// Broadcast Loop
			for {
				pktType, payload, err := protocol.ReadPacket(conn)
				if err != nil {
					if err != io.EOF {
						log.Printf("Broadcaster Read Error: %v", err)
					}
					break
				}

				pkt := &Packet{Type: pktType, Payload: payload}

				// Fan-out
				b.mu.RLock()
				for ch := range b.listeners {
					// Non-blocking send to avoid stalling
					select {
					case ch <- pkt:
					default:
						// Drop frame if listener slow
					}
				}
				b.mu.RUnlock()
			}
			
			b.mu.Lock()
			if b.activeConn == conn {
				b.activeConn = nil
			}
			b.mu.Unlock()
			
			conn.Close()
			log.Println("Broadcaster: Mock AI Disconnected.")
		}
	}()
}

func (b *UDSBroadcaster) SendToWorker(pktType byte, payload []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.activeConn == nil {
		return io.ErrClosedPipe
	}
	// Direct write to the worker socket
	// Note: protocol.WritePacket is thread-safe if we assume only one writer (Server) -> Worker
	// If Start() loop also writes (unlikely), we might need lock on conn.
	// For now, Start() only READS. SendToWorker WRITES.
	// net.Conn is thread-safe for concurrent Read and Write.
	return protocol.WritePacket(b.activeConn, pktType, payload)
}

func (b *UDSBroadcaster) Subscribe() chan *Packet {
	ch := make(chan *Packet, 100)
	b.mu.Lock()
	b.listeners[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *UDSBroadcaster) Unsubscribe(ch chan *Packet) {
	b.mu.Lock()
	delete(b.listeners, ch)
	close(ch) // Close channel
	b.mu.Unlock()
}
