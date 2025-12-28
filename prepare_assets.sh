#!/bin/bash

# 确保目录存在
mkdir -p assets

echo "========================================"
echo "Generating VP8 (Video) and Opus (Audio)"
echo "========================================"

# --- 1. 处理 Idle ---
if [ -f "assets/idle.mp4" ]; then
    echo "[Idle] Generating Video (IVF)..."
    # 你的原版命令，VP8 编码
    ffmpeg -y -i assets/idle.mp4 \
        -c:v libvpx -b:v 2000k \
        -g 25 -keyint_min 25 -auto-alt-ref 0 \
        -f ivf assets/idle.ivf

    echo "[Idle] Generating Audio (OGG)..."
    # 提取 Opus 音频，切片为 20ms
    ffmpeg -y -i assets/idle.mp4 \
        -c:a libopus -b:a 48k -page_duration 20000 \
        -vn assets/idle.ogg
else
    echo "Error: assets/idle.mp4 not found"
fi

# --- 2. 处理 Talking (如果有) ---
if [ -f "assets/talking.mp4" ]; then
    echo "[Talking] Generating Video (IVF)..."
    ffmpeg -y -i assets/talking.mp4 \
        -c:v libvpx -b:v 2000k \
        -g 25 -keyint_min 25 -auto-alt-ref 0 \
        -f ivf assets/talking.ivf

    echo "[Talking] Generating Audio (OGG)..."
    ffmpeg -y -i assets/talking.mp4 \
        -c:a libopus -b:a 48k -page_duration 20000 \
        -vn assets/talking.ogg
fi

echo "Done! Restart your Go program."