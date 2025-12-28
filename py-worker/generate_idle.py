import subprocess
import torch
import numpy as np
import os

# Configuration matching the Live Stream exactly
WIDTH = 512
HEIGHT = 768
FPS = 25
OUTPUT_FILE = "../assets/idle.h264"

def generate_idle():
    print(f"Generating aligned idle asset to {OUTPUT_FILE}...")
    
    # FFmpeg command matching the User's Log (CPU Encoding, Low Latency, Baseline)
    command = [
        'ffmpeg', '-y',
        '-f', 'rawvideo',
        '-vcodec', 'rawvideo',
        '-pix_fmt', 'rgb24',
        '-s', f'{WIDTH}x{HEIGHT}',
        '-r', str(FPS),
        '-i', '-',
        
        '-c:v', 'libx264',
        '-pix_fmt', 'yuv420p',
        '-profile:v', 'baseline',
        '-slices', '1',
        '-preset', 'ultrafast',
        '-tune', 'zerolatency',
        '-g', str(FPS * 2),
        '-b:v', '2M',
        '-bufsize', '4M',
        '-f', 'h264',
        OUTPUT_FILE  # Write to file directly
    ]

    process = subprocess.Popen(
        command,
        stdin=subprocess.PIPE,
        stderr=subprocess.PIPE
    )

    # Generate 2 seconds (50 frames) of pure black (or could be an image)
    # Using Torch to mitigate any numpy alignment issues, matching live pipeline
    frames_tensor = torch.zeros((50, 3, HEIGHT, WIDTH), dtype=torch.uint8)
    
    # If you want a specific color (e.g. dark gray)
    # frames_tensor[:] = 20

    # Permute to HWC for FFmpeg (Height, Width, Channel)
    # Shape: [50, 768, 512, 3]
    frames_np = frames_tensor.permute(0, 2, 3, 1).numpy()
    
    try:
        process.stdin.write(frames_np.tobytes())
        process.stdin.close()
        process.wait()
        print("✅ Generation Complete!")
    except Exception as e:
        print(f"❌ Error: {e}")
        process.kill()

if __name__ == "__main__":
    generate_idle()
