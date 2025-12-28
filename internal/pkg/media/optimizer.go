package media

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureOptimized checks if an optimized version of the asset exists.
// If not, it transcodes it using FFmpeg.
// Returns the path to the optimized asset.
func EnsureOptimized(inputPath string) (string, error) {
	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(absInput)
	base := filepath.Base(absInput)
	ext := filepath.Ext(base)
	name := base[:len(base)-len(ext)]
	
	// Define Optimized Output Name
	outputPath := filepath.Join(dir, name+"_opt.mp4")

	// Check if exists
	if _, err := os.Stat(outputPath); err == nil {
		log.Printf("Optimized asset found: %s", outputPath)
		return outputPath, nil
	}

	log.Printf("Optimizing asset %s -> %s (This may take a moment)...", inputPath, outputPath)

	// Command to transcode to 576x704 H.264 Baseline
	// We optimize for compatibility and decoding speed.
	cmd := exec.Command("ffmpeg",
		"-y",
		"-i", absInput,
		"-c:v", "libx264",
		"-preset", "fast", // Good enough for offline transcode
		"-profile:v", "baseline",
		"-s", "576x704", // Force resolution
		"-r", "25",
		"-pix_fmt", "yuv420p",
		"-g", "50",
		"-keyint_min", "50",
		"-sc_threshold", "0",
		"-c:a", "aac", // Keep audio as AAC for MP4 container
		"-b:a", "128k",
		outputPath,
	)
	
    // Capture output for debugging if it fails
    output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("optimization failed: %v\nOutput: %s", err, string(output))
	}

	log.Println("Optimization complete.")
	return outputPath, nil
}
