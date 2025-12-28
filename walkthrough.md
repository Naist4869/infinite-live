# InfiniteLive Phase 1 Walkthrough

## Overview
Phase 1 (Go WebRTC Streamer) implementation is complete. The system is set up with a Clean Architecture, ready to stream H.264 files via WebRTC.

## Changes Made
- **Domain Layer**: Defined `AvatarState`, `VideoFrame` and interfaces.
- **Infrastructure**: Configured Pion WebRTC.
- **Adapters**: Implemented `FileLoopSource` (reads local H.264) and `PionPublisher` (pushes to WebRTC).
- **Core Logic**: Implemented `LiveInteractor` state machine.
- **Entry Point**: `cmd/server/main.go` wires everything up.

## Verification Steps (Manual)
Since the agent's internal `wsl` command is encountering issues, please run the following commands in your **WSL terminal** (`zsh`):

### 1. Build & Init
Navigate to the project directory:
```zsh
cd ~/src/infinite-live
```

Initialize the module and download dependencies:
```zsh
go mod init infinite-live
go get github.com/pion/webrtc/v4
go mod tidy
```

### 2. Prepare Assets (Crucial)
We need to pre-transcode the MP4 assets to raw H.264 to avoid runtime lag and artifacts.
Run the provided helper script:
```zsh
# Make it executable
chmod +x prepare_assets.sh
# Run it (Requires ffmpeg installed)
./prepare_assets.sh
```
*Output:* Should say "Done! Assets are ready in ./assets/" creating `idle_processed.h264` and `talking_processed.h264`.

### 2. Run Server
Start the server (Direct MP4 Mode):
```zsh
go build -o server cmd/server/main.go
go run cmd/server/main.go
```
*Expected Output:* `Server listening on :8080`

### 4. Test Client
1. Open a browser (Chrome/Edge recommended).
2. Visit `http://localhost:8080`.
3. Click **Start Stream**.
4. You should see the video playing (or black/green screen if the H.264 file is empty/invalid).

## Phase 2 Verification (Go Mock AI)
This phase introduces the "Brain" using a Go-based mock worker communicating via Unix Domain Sockets.

### 1. Build Mock AI
Open a new terminal tab (or split pane) and build the mock worker:
```zsh
cd ~/src/infinite-live
# Ensure ffmpeg is installed: sudo apt install ffmpeg
go mod tidy # Fetch new dependencies (h264reader)
go build -o mock_worker cmd/mock_ai/main.go
# This creates a 'mock_worker' binary
```

### 2. Run the System
You need two terminals running simultaneously.

**Terminal 1 (Server):**
```zsh
go run cmd/server/main.go
# Output: Server listening on :8080
```

**Terminal 2 (Mock Worker):**
Wait for the server to start, then run:
```zsh
./mock_worker
# Output: Mock AI: Connected to Engine!
```
*Note: The worker now uses `ffmpeg` to read `assest/infinitetalk_output.mp4` in real-time. Ensure this file exists.*

### 3. Verify in Browser
1.  Open `http://localhost:8080`.
2.  Click **Start Stream**.
3.  Initially, you will see the "Idle" video (looped).
4.  **Wait 5 seconds**: The server logic is hardcoded to switch to "Talking" state (UDS source) after 5 seconds of connection.
5.  You should see the video stream continue (simulated by Mock AI). Since Mock AI reuses the same file, it might look similar, but check the server logs:
    -   Server Log: `Switching to Talking State (UDS)...`
    -   Server Log: `UDS Receiver: Worker connected!`

## Phase 3 Verification (Interactive AI)
This phase enables MP4 playback via FFmpeg and user interaction.

### 1. Update Dependencies
We added Audio support (Opus/Ogg).
```zsh
go mod tidy
```

### 2. Prepare Assets (Crucial)
Run the script to generate optimized Raw Assets (.h264 Video, .ogg Audio):
```zsh
./prepare_assets.sh
```

## Running the System (Doubao Integration)

### 1. Build
```zsh
go build -o server cmd/server/main.go
go build -o doubao_worker cmd/doubao_worker/*.go
```

### 2. Set Credentials
```zsh
export DOUBAO_APPID="your_appid"
export DOUBAO_TOKEN="your_token"
```

### 3. Run
**Terminal 1 (Server):**
```zsh
./server
```

**Terminal 2 (Worker):**
```zsh
./doubao_worker
```

### 4. Verify
1. Open `http://localhost:8080`.
2. Click **Start Stream**.
3. Type a message in the input box (e.g., "你好") and click **Send**.
4. The avatar should start waiting/talking once the response is received. Open a terminal and send a specific POST request:
    ```zsh
    curl -X POST http://localhost:8080/comment
    ```
4.  **Observe**:
    -   **Immediate**: Logs show `Switching to Listening/Thinking`.
    -   **After 2s**: Logs show `Switching to Talking`.
    -   **Video**: Should switch to `assest/infinitetalk_output.mp4` (streamed from mock worker).
    -   **Finish**: When the talking video ends (or loop), it might switch back or loop depending on mock logic.

## Summary
You now have a complete end-to-end "Skeleton" of the InfiniteLive system:
-   **Go Engine**: Handles WebRTC, State Machine, and FFmpeg transcoding.
-   **Mock Brain**: Simulates the Python worker streaming generated content via UDS.
-   **Integration**: Seamless switching between locally idle video and remotely generated video.
