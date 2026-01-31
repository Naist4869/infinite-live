package infrastructure

import (
	"infinite-live/internal/pkg/protocol"
	"io"
	"log"
	"net"
	"sync"
)

type Packet struct {
	Type    byte
	Payload []byte
}

// WorkerSession 保持极简
type WorkerSession struct {
	conn net.Conn
	mu   sync.Mutex // 互斥锁：确保一次完整的 Packet 写入是原子的
}

type UDSBroadcaster struct {
	server       *UDSServer
	listeners    map[chan *Packet]struct{}
	mu           sync.RWMutex
	workerConns  sync.Map // map[*WorkerSession]struct{}
	sessionQueue chan chan *Packet
	stopCh       chan struct{}
}

func NewUDSBroadcaster(server *UDSServer) *UDSBroadcaster {
	return &UDSBroadcaster{
		server:       server,
		listeners:    make(map[chan *Packet]struct{}),
		sessionQueue: make(chan chan *Packet, 100),
		stopCh:       make(chan struct{}),
	}
}

func (b *UDSBroadcaster) Start() {
	go b.runSequencer()

	go func() {
		for {
			conn, err := b.server.Accept()
			if err != nil {
				log.Printf("Broadcaster Accept Error: %v", err)
				return
			}
			log.Println("Broadcaster: New Connection Accepted.")

			session := &WorkerSession{conn: conn}
			b.workerConns.Store(session, struct{}{})

			sessionCh := make(chan *Packet, 2000)

			select {
			case b.sessionQueue <- sessionCh:
				go b.handleReadLoop(session, sessionCh)
			default:
				log.Println("Broadcaster: Queue full, dropping connection")
				conn.Close()
				b.workerConns.Delete(session)
			}
		}
	}()
}

// handleReadLoop 负责读取 (跟之前一样)
func (b *UDSBroadcaster) handleReadLoop(session *WorkerSession, outCh chan *Packet) {
	defer func() {
		b.workerConns.Delete(session)
		session.conn.Close()
		close(outCh)
		log.Println("Broadcaster: Connection Closed/Removed.")
	}()

	for {
		pktType, payload, err := protocol.ReadPacket(session.conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("Session Read Error: %v", err)
			}
			break
		}
		outCh <- &Packet{Type: pktType, Payload: payload}
	}
}

// SendToWorker 变得非常优雅且简单
func (b *UDSBroadcaster) SendToWorker(pktType byte, payload []byte) error {
	b.workerConns.Range(func(key, value interface{}) bool {
		session := key.(*WorkerSession)

		// ❌ 移除 go func
		// ❌ 移除 channel
		// ✅ 直接加锁 -> 写入 -> 解锁
		// 这样既保证了包内原子性，又严格保证了调用顺序

		session.mu.Lock()
		defer session.mu.Unlock()

		// 因为这里是同步调用，包1没写完，绝对不会开始写包2
		if err := protocol.WritePacket(session.conn, pktType, payload); err != nil {
			// 如果写入错误，通常意味着连接断了
			// 可以在这里做简单的日志，handleReadLoop 会负责清理资源
			// log.Printf("Write error: %v", err)
		}

		return true
	})
	return nil
}

// ... runSequencer, broadcast, Subscribe, Unsubscribe 保持不变 ...
func (b *UDSBroadcaster) runSequencer() {
	log.Println("Broadcaster: Sequencer Started.")
	for sessionCh := range b.sessionQueue {
		// 只要 sessionCh 没关闭，就一直读（Drain the session）
		// 这保证了当前视频没播完，绝对不会切到下一个
		for pkt := range sessionCh {
			b.broadcast(pkt)
		}
	}
}

func (b *UDSBroadcaster) broadcast(pkt *Packet) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.listeners {
		select {
		case ch <- pkt:
		default:
			log.Println("Broadcaster: Listener Dropped.")
		}
	}
}

func (b *UDSBroadcaster) Subscribe() chan *Packet {
	ch := make(chan *Packet, 2000)
	b.mu.Lock()
	b.listeners[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *UDSBroadcaster) Unsubscribe(ch chan *Packet) {
	b.mu.Lock()
	delete(b.listeners, ch)
	close(ch)
	b.mu.Unlock()
}
