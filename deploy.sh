#!/bin/bash
set -e

# ================= 配置区域 =================
REMOTE_HOST="infinite"
REMOTE_USER="lan"
REMOTE_DIR="/home/lan/LightX2V/go"
SSH_PORT="22"

# 1. 本地环境变量检查
if [ -z "$DOUBAO_APPID" ]; then
  echo "❌ Error: 环境变量 DOUBAO_APPID 未在本地设置。"
  exit 1
fi

if [ -z "$DOUBAO_TOKEN" ]; then
  echo "❌ Error: 环境变量 DOUBAO_TOKEN 未在本地设置。"
  exit 1
fi

echo "🚀 Starting Deployment to $REMOTE_HOST:$SSH_PORT..."

# 2. Build Binaries
echo "📦 Building 'server'..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server cmd/server/main.go

echo "📦 Building 'doubao_worker'..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o doubao_worker cmd/doubao_worker/*.go

# 3. Stop Old Processes (修复：只杀二进制，不杀 tail)
echo "🔪 Stopping running processes..."

# 重点修改在这里：
# 1. 使用 "^" 符号，表示匹配命令行开头。
# 2. tail 命令是以 "tail" 开头的，所以不会被选中。
# 3. 我们的程序是以 "/home/..." 开头的，所以会被精准命中。
# pkill -f "^/home/lan/LightX2V/go/server"; pkill -f "^/home/lan/LightX2V/go/doubao_worker"
ssh -p $SSH_PORT $REMOTE_HOST "pkill -f \"^$REMOTE_DIR/server\"; pkill -f \"^$REMOTE_DIR/doubao_worker\"" || true

echo "⏳ Waiting 3s for ports to release..."
sleep 3

echo "📦 Moving old binaries to .old..."
ssh -p $SSH_PORT $REMOTE_HOST "mv $REMOTE_DIR/server $REMOTE_DIR/server.old 2>/dev/null; mv $REMOTE_DIR/doubao_worker $REMOTE_DIR/doubao_worker.old 2>/dev/null" || true

echo "📂 Creating remote directory..."
ssh -p $SSH_PORT $REMOTE_HOST "mkdir -p $REMOTE_DIR"

# 4. Transfer Files
echo "Tx Transferring binaries..."
scp -P $SSH_PORT server doubao_worker $REMOTE_HOST:$REMOTE_DIR/

echo "Tx Transferring assets..."
scp -P $SSH_PORT -r assets static $REMOTE_HOST:$REMOTE_DIR/

# 5. Set Permissions
echo "mmm Setting permissions..."
ssh -p $SSH_PORT $REMOTE_HOST "chmod +x $REMOTE_DIR/server $REMOTE_DIR/doubao_worker"

# 6. Restart Processes
echo "🔄 Restarting processes..."

REMOTE_ENV_STR="export DOUBAO_APPID='$DOUBAO_APPID' && export DOUBAO_TOKEN='$DOUBAO_TOKEN'"

# --- 启动 Server ---
echo "1️⃣  Starting Server..."
# 使用绝对路径启动，配合上面的 pkill ^...
ssh -p $SSH_PORT -n -f $REMOTE_HOST "cd $REMOTE_DIR && nohup $REMOTE_DIR/server > server.log 2>&1 < /dev/null &"

echo "⏳ Waiting 5s for Server to initialize..."
sleep 5

# --- 启动 Worker ---
echo "2️⃣  Starting Doubao Worker..."
ssh -p $SSH_PORT -n -f $REMOTE_HOST "cd $REMOTE_DIR && $REMOTE_ENV_STR && nohup $REMOTE_DIR/doubao_worker > worker.log 2>&1 < /dev/null &"

echo "✅ Deployment & Restart Complete!"
echo "   Logs:"
echo "   ssh $REMOTE_HOST 'tail -f $REMOTE_DIR/server.log'"
echo "   ssh $REMOTE_HOST 'tail -f $REMOTE_DIR/worker.log'"