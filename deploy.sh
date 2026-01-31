#!/bin/bash
set -e

# ================= 配置区域 =================
REMOTE_HOST="infinite"
REMOTE_USER="lan"
REMOTE_DIR="/home/lan/LightX2V/go"
SSH_PORT="22"

# 0. 解析命令行参数 (默认为 doubao)
WORKER_TYPE=${1:-doubao}

# 校验参数并设置变量
if [ "$WORKER_TYPE" == "doubao" ]; then
    WORKER_SRC="cmd/doubao_worker/*.go"
    WORKER_BIN="doubao_worker"
    echo "🎯 Mode Selected: Doubao Worker"
elif [ "$WORKER_TYPE" == "qwen" ]; then
    WORKER_SRC="cmd/cosyvoice/*.go"
    WORKER_BIN="qwen_worker"
    echo "🎯 Mode Selected: Qwen Worker"

else
    echo "❌ Error: Invalid worker type. Usage: $0 [doubao|qwen]"
    exit 1
fi

# 1. 检查 Doppler 命令
if ! command -v doppler &> /dev/null; then
    echo "❌ Error: 未找到 doppler 命令，请先安装。"
    exit 1
fi

echo "🚀 Starting Deployment to $REMOTE_HOST:$SSH_PORT..."

# 2. Build Binaries
echo "📦 Building binaries..."
# 编译 Server (总是编译)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server cmd/server/*.go

# 编译选定的 Worker
echo "🔨 Building $WORKER_BIN from $WORKER_SRC..."
GOOS=linux GOARCH=amd64 go build -o $WORKER_BIN $WORKER_SRC

# 3. Stop Old Processes
echo "🔪 Stopping running processes..."
# 无论当前部署哪个，把可能存在的 server 和两种 worker 都杀掉，防止冲突
ssh -p $SSH_PORT $REMOTE_HOST "pkill -f \"^$REMOTE_DIR/server\"; pkill -f \"^$REMOTE_DIR/doubao_worker\"; pkill -f \"^$REMOTE_DIR/qwen_worker\"" || true

echo "⏳ Waiting 3s for ports to release..."
sleep 3

# 备份旧文件
echo "📦 Moving old binaries to .old..."
# 尝试备份当前部署的二进制文件
ssh -p $SSH_PORT $REMOTE_HOST "mv $REMOTE_DIR/server $REMOTE_DIR/server.old 2>/dev/null; mv $REMOTE_DIR/$WORKER_BIN $REMOTE_DIR/$WORKER_BIN.old 2>/dev/null" || true

echo "📂 Creating remote directory..."
ssh -p $SSH_PORT $REMOTE_HOST "mkdir -p $REMOTE_DIR"

# 4. Transfer Files (Binaries & Assets)
echo "Tx Transferring binaries and assets..."
# 只上传 server 和 选定的 worker
scp -P $SSH_PORT server $WORKER_BIN $REMOTE_HOST:$REMOTE_DIR/
scp -P $SSH_PORT -r assets static $REMOTE_HOST:$REMOTE_DIR/

# =======================================================
# 🔥 核心修改：通过管道直接流式传输密钥，本机不落地 🔥
# =======================================================
echo "🔐 Streaming secrets to remote server (No local file created)..."
doppler secrets download --no-file --format env | ssh -p $SSH_PORT $REMOTE_HOST "cat > $REMOTE_DIR/.env && chmod 600 $REMOTE_DIR/.env"

# 5. Set Permissions
echo "mmm Setting permissions..."
ssh -p $SSH_PORT $REMOTE_HOST "chmod +x $REMOTE_DIR/server $REMOTE_DIR/$WORKER_BIN"

# 6. Restart Processes
echo "🔄 Restarting processes..."

REMOTE_ENV_LOADER="set -a && source $REMOTE_DIR/.env && set +a"

# --- 启动 Server ---
echo "1️⃣  Starting Server..."
ssh -p $SSH_PORT -n -f $REMOTE_HOST "cd $REMOTE_DIR && $REMOTE_ENV_LOADER && nohup $REMOTE_DIR/server > server.log 2>&1 < /dev/null &"

echo "⏳ Waiting 5s for Server to initialize..."
sleep 5

# --- 启动 Selected Worker ---
echo "2️⃣  Starting $WORKER_BIN..."
# 日志文件名也改为对应 Worker 名称
ssh -p $SSH_PORT -n -f $REMOTE_HOST "cd $REMOTE_DIR && $REMOTE_ENV_LOADER && nohup $REMOTE_DIR/$WORKER_BIN > ${WORKER_BIN}.log 2>&1 < /dev/null &"

echo "✅ Deployment & Restart Complete!"
echo "   Logs:"
echo "   ssh -p $SSH_PORT $REMOTE_HOST 'tail -f $REMOTE_DIR/server.log'"
echo "   ssh -p $SSH_PORT $REMOTE_HOST 'tail -f $REMOTE_DIR/${WORKER_BIN}.log'"