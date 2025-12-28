package infrastructure

import (
	"net"
	"os"
)

type UDSServer struct {
	listener net.Listener
}

func NewUDSServer(socketPath string) (*UDSServer, error) {
	// Cleanup old socket
	if _, err := os.Stat(socketPath); err == nil {
		os.Remove(socketPath)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	return &UDSServer{
		listener: listener,
	}, nil
}

func (s *UDSServer) Accept() (net.Conn, error) {
	return s.listener.Accept()
}

func (s *UDSServer) Close() {
	s.listener.Close()
}
