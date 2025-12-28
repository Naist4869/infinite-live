import time
import os

class MockGenerator:
    def __init__(self, asset_path="../assets/idle.h264"):
        # For now, we reuse the idle file or a specific 'talking' file if available
        # Adjust path as needed relative to execution dir
        self.asset_path = asset_path
        if not os.path.exists(self.asset_path):
            # Fallback for running from py-worker dir
            self.asset_path = "../assets/idle.h264"
        
        self.content = b""
        if os.path.exists(self.asset_path):
            with open(self.asset_path, "rb") as f:
                self.content = f.read()
        else:
             print(f"Warning: Asset {self.asset_path} not found. Streaming empty.")

    def generate_frames(self):
        """
        Yields NALUs indefinitely.
        Simulates Generative AI latency.
        """
        cursor = 0
        while True:
            # Simulate inference time (12.5fps = 80ms, but let's go faster for smooth mock)
            time.sleep(0.04) 

            if not self.content:
                yield b'\x00\x00\x00\x01' # Dummy
                continue
            
            if cursor >= len(self.content):
                cursor = 0 # Loop
            
            # Simple NAL parser (same logic as Go side for consistency)
            start = cursor
            
            # Find next start code
            next_start = -1
            # Search after start+4
            search_start = start + 4
            if search_start < len(self.content):
                # Naive search
                # We can cheat since we know it's a valid file if we read it
                # But for robustness let's just send fixed chunks if parsing is hard in pure python without regex/KMP
                # Actually, sending fixed arbitrary chunks to H.264 decoder usually fails
                # Let's try to do a real basic split
                res = self.find_start_code(self.content, search_start)
                if res != -1:
                    next_start = res
            
            if next_start == -1:
                chunk = self.content[start:]
                cursor = len(self.content)
            else:
                chunk = self.content[start:next_start]
                cursor = next_start
            
            yield chunk

    def find_start_code(self, data, start_idx):
        # Look for 00 00 00 01
        # Slow in python loop, but okay for mock
        try:
            # We assume NAL start with 00 00 00 01 or 00 00 01
            # Let's search for 0x00 00 01
            idx = data.find(b'\x00\x00\x01', start_idx)
            if idx != -1:
                # check for preceding 00
                if idx > 0 and data[idx-1] == 0x00:
                    return idx - 1
                return idx
        except:
            pass
        return -1
