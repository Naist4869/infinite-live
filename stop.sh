#!/bin/bash
set -e

# ================= 配置区域 =================
# 保持与部署脚本一致的配置
REMOTE_HOST="infinite"
REMOTE_USER="lan"
REMOTE_DIR="/home/lan/LightX2V/go"
SSH_PORT="22"

echo "🔪 Connect to $REMOTE_HOST:$SSH_PORT to stop processes..."

# ================= 核心停止逻辑 =================
# 逻辑说明：
# 1. 匹配完整路径，防止误杀其他无关进程
# 2. 同时尝试停止 server, doubao_worker, qwen_worker
# 3. || true 确保如果进程本身没在运行，脚本不会报错退出
ssh -p $SSH_PORT $REMOTE_HOST "pkill -f \"^$REMOTE_DIR/server\"; pkill -f \"^$REMOTE_DIR/doubao_worker\"; pkill -f \"^$REMOTE_DIR/qwen_worker\"" || true

echo "⏳ Waiting for processes to exit..."
sleep 2

# ================= (可选) 检查是否已停止 =================
echo "🔍 Checking status..."
ssh -p $SSH_PORT $REMOTE_HOST "pgrep -f \"^$REMOTE_DIR/server\" > /dev/null && echo '⚠️ Server is still running' || echo '✅ Server stopped'"
ssh -p $SSH_PORT $REMOTE_HOST "pgrep -f \"^$REMOTE_DIR/doubao_worker\" > /dev/null && echo '⚠️ Doubao Worker is still running' || echo '✅ Doubao Worker stopped'"
ssh -p $SSH_PORT $REMOTE_HOST "pgrep -f \"^$REMOTE_DIR/qwen_worker\" > /dev/null && echo '⚠️ Qwen Worker is still running' || echo '✅ Qwen Worker stopped'"

echo "👋 All Stop Operations Completed."