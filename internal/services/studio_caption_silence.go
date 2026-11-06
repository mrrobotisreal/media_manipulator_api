package services

import (
	"context"
	"regexp"
	"strconv"
)

// Trailing-silence detection for studio captions (MM121): whisper invents
// "Thank you." and repeats lines over a silent tail; words starting inside a
// final silence are dropped (HyperFrames captions rule: silencedetect at
// -35 dB, 0.6 s; real speech sits around -16 to -26 dB).

var silenceStartRe = regexp.MustCompile(`silence_start: (-?[0-9.]+)`)
var silenceEndRe = regexp.MustCompile(`silence_end: (-?[0-9.]+)`)

// DetectTrailingSilence returns where the audio's final silence starts, or 0
// when the file does not end in silence (or ffmpeg fails: never drop words on
// an error).
func DetectTrailingSilence(ctx context.Context, path string) float64 {
	_, stderr, err := runCommand(ctx, "ffmpeg", "-hide_banner", "-nostats", "-i", path, "-af", "silencedetect=noise=-35dB:d=0.6", "-f", "null", "-")
	if err != nil {
		return 0
	}
	return parseTrailingSilence(stderr)
}

// parseTrailingSilence reads silencedetect output: the last silence_start
// with no silence_end after it is a silence running to the end of the file.
func parseTrailingSilence(stderr string) float64 {
	starts := silenceStartRe.FindAllStringSubmatchIndex(stderr, -1)
	if len(starts) == 0 {
		return 0
	}
	last := starts[len(starts)-1]
	if silenceEndRe.MatchString(stderr[last[1]:]) {
		return 0
	}
	v, err := strconv.ParseFloat(stderr[last[2]:last[3]], 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}
