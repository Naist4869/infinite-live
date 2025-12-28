package main

import (
	"log"
	"net"
	"os"
	"sync"
	"time"

	"infinite-live/internal/adapter/file"
	"infinite-live/internal/domain" // Added
	"infinite-live/internal/pkg/protocol"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

const SocketPath = "/tmp/infinite-live.sock"

func main() {
	log.Println("Mock AI: Starting...")
	videoPath := "assets/talking.ivf" // 改为 ivf
	audioPath := "assets/talking.ogg"

	for {
		// Connect to Engine
		log.Println("Connecting to Engine...")
		conn, err := net.Dial("unix", SocketPath)
		if err != nil {
			log.Printf("Failed to connect: %v. Retrying in 2s...", err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Println("Mock AI: Connected!")

		var wg sync.WaitGroup
		var mu sync.Mutex

		wg.Add(2)

		// --- Video Stream (Goroutine) ---
		go func() {
			defer wg.Done()

			vSource, err := file.NewSequentialReader(videoPath, domain.StateTalking)
			if err != nil {
				log.Printf("Video Source Failed: %v", err)
				return
			}
			defer vSource.Close()

			// Pacing (Send slightly faster than 40ms to keep buffer healthy)
			// Target 40ms (25fps). Sending at 33ms is safe.
			ticker := time.NewTicker(33 * time.Millisecond)
			defer ticker.Stop()

			for range ticker.C {
				frame, err := vSource.NextFrame()
				if err != nil {
					break
				}
				frame.Type = protocol.PacketTypeVideo

				mu.Lock()
				err = protocol.WritePacket(conn, protocol.PacketTypeVideo, frame.Data)
				mu.Unlock()

				if err != nil {
					log.Printf("Video Write Failed: %v", err)
					break
				}
			}
			log.Println("Video Stream Finished")
		}()

		// --- Audio Stream (Goroutine) ---
		go func() {
			defer wg.Done()

			f, err := os.Open(audioPath)
			if err != nil {
				log.Printf("Audio File Failed: %v", err)
				return
			}
			defer f.Close()

			ogg, _, err := oggreader.NewWith(f)
			if err != nil {
				log.Printf("Ogg Reader Failed: %v", err)
				return
			}

			// Pacing for Audio (20ms standard for Opus)
			// Send slightly faster (15ms) to keep buffer full
			ticker := time.NewTicker(15 * time.Millisecond)
			defer ticker.Stop()

			for range ticker.C {
				payload, _, err := ogg.ParseNextPage()
				if err != nil {
					break // EOF
				}

				mu.Lock()
				err = protocol.WritePacket(conn, protocol.PacketTypeAudio, payload)
				mu.Unlock()
				if err != nil {
					break
				}
			}
			log.Println("Audio Stream Finished")
		}()

		wg.Wait()

		// Send EOS
		mu.Lock()
		protocol.WritePacket(conn, protocol.PacketTypeVideo, []byte{})
		mu.Unlock()

		conn.Close()
		log.Println("Session Done. Waiting 100ms before next loop...")
		time.Sleep(100 * time.Millisecond)
	}
}
