import socket
import struct
import os
import time
import logging

# Configure logging
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')
logger = logging.getLogger(__name__)

class UDSStreamer:
    def __init__(self, socket_path='/tmp/infinite_live.sock'):
        self.socket_path = socket_path
        self.sock = None
        self.app_running = True

    def start_server(self):
        # Clean up old socket
        if os.path.exists(self.socket_path):
            os.remove(self.socket_path)

        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(self.socket_path)
        self.sock.listen(1)
        logger.info(f"Listening on {self.socket_path}...")

        while self.app_running:
            logger.info("Waiting for connection...")
            conn, _ = self.sock.accept()
            logger.info("Go client connected!")
            try:
                self.handle_client(conn)
            except Exception as e:
                logger.error(f"Connection error: {e}")
            finally:
                conn.close()

    def handle_client(self, conn):
        from generator import MockGenerator
        gen = MockGenerator()
        
        logger.info("Starting stream loop...")
        for frame in gen.generate_frames():
            # Protocol: 
            # 1. Length (4 bytes, Big Endian)
            # 2. Payload (Content)
            length = len(frame)
            header = struct.pack('>I', length)
            
            try:
                conn.sendall(header)
                conn.sendall(frame)
                # logger.debug(f"Sent frame size: {length}")
            except BrokenPipeError:
                logger.info("Client disconnected.")
                break
            
    def stop(self):
        self.app_running = False
        if self.sock:
            self.sock.close()
        if os.path.exists(self.socket_path):
            os.remove(self.socket_path)

if __name__ == "__main__":
    server = UDSStreamer()
    try:
        server.start_server()
    except KeyboardInterrupt:
        server.stop()
