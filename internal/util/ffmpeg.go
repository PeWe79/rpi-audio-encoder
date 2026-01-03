package util

import "os/exec"

// ResolveFFmpegPath returns the path to the FFmpeg binary, or empty string if not found.
func ResolveFFmpegPath(customPath string) string {
	if customPath != "" {
		if _, err := exec.LookPath(customPath); err == nil {
			return customPath
		}
		return ""
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	return path
}
