package services

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StitchAudioToVideoService mixes one base video with up to a handful of
// additional audio tracks — voiceovers, music, narration — and emits a single
// MP4. It supports two top-level modes:
//
//   - "mix":     keep the original audio (if any) and overlay the new tracks
//   - "replace": drop the original audio and use only the new tracks
//
// Each added audio track has its own volume scalar (0..4), start offset in
// seconds, and an optional "loop" flag for short backing tracks that should
// extend to the video duration.
type StitchAudioToVideoService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewStitchAudioToVideoService(cfg *config.Config, jm *JobManager) *StitchAudioToVideoService {
	return &StitchAudioToVideoService{cfg: cfg, jobManager: jm}
}

// StitchAudioTrack is one of the user-supplied audio files. Path is the
// already-staged path on disk; the handler is responsible for sanitization
// and for cleaning the file up via the standard cleanup worker.
type StitchAudioTrack struct {
	Path     string
	Volume   float64
	DelaySec float64
	Loop     bool
}

// StitchAudioRequest captures the validated, ready-to-run job parameters.
type StitchAudioRequest struct {
	Mode                string // "mix" | "replace"
	TrimToVideoDuration bool
	Tracks              []StitchAudioTrack
}

// Stitch runs FFmpeg with a programmatically-built filter_complex graph. We
// never join the filter graph from raw user strings — every component comes
// from a constrained set of validated primitives (volume floats, delay floats,
// "amix" / "amerge", input indexes). That keeps this safe against argument
// injection even though the user controls some of the inputs.
func (s *StitchAudioToVideoService) Stitch(ctx context.Context, job *models.ConversionJob, videoPath, outputPath string, req StitchAudioRequest) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	if len(req.Tracks) == 0 {
		return errors.New("at least one audio track is required")
	}
	hasOriginalAudio := ffprobeHasStream(ctx, videoPath, "a")
	includeOriginal := req.Mode == "mix" && hasOriginalAudio

	s.progress(job.ID, 10)

	// Build the FFmpeg command:
	//   inputs:
	//     0: base video
	//     1..N: added audio tracks
	//
	// Filter graph (per track i):
	//     [i:a] adelay=DELAY_MS|DELAY_MS, volume=V [a_i];
	//   then either:
	//     [a_1][a_2]...[a_N] amix=inputs=N:duration=longest [aout]
	//   or if includeOriginal:
	//     [0:a] amix into the same chain.
	args := []string{"-y"}
	for i, track := range req.Tracks {
		// -stream_loop -1 makes a short backing track loop until the video ends.
		// We apply it pre-input, mirroring FFmpeg's documented usage.
		if track.Loop {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args, "-i", track.Path)
		_ = i
	}
	// Video input goes first conceptually, but FFmpeg requires inputs in argv
	// order; we re-jigger so input 0 is the video, then audio tracks follow.
	// Rebuild args with the proper input order:
	args = []string{"-y", "-i", videoPath}
	for _, track := range req.Tracks {
		if track.Loop {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args, "-i", track.Path)
	}

	filterParts := make([]string, 0, len(req.Tracks)+2)
	mixLabels := make([]string, 0, len(req.Tracks)+1)
	for i, track := range req.Tracks {
		inputIdx := i + 1 // input 0 is the video
		label := fmt.Sprintf("a%d", i)
		delayMS := int(track.DelaySec * 1000)
		// Build:
		//   [N:a] adelay=DMS|DMS, volume=V [aN]
		var sb strings.Builder
		fmt.Fprintf(&sb, "[%d:a]", inputIdx)
		if delayMS > 0 {
			// adelay needs one value per channel; we cap at stereo and pass the
			// same delay twice, which works for both mono and stereo sources.
			fmt.Fprintf(&sb, "adelay=%d|%d,", delayMS, delayMS)
		}
		fmt.Fprintf(&sb, "volume=%s[%s]", formatVolumeArg(track.Volume), label)
		filterParts = append(filterParts, sb.String())
		mixLabels = append(mixLabels, "["+label+"]")
	}

	mixInputs := len(req.Tracks)
	if includeOriginal {
		mixInputs++
		mixLabels = append([]string{"[0:a]"}, mixLabels...)
	}

	if mixInputs == 1 {
		// Pass-through: rename the single track to [aout] so the mapping
		// below works whether or not we ran amix.
		filterParts = append(filterParts, fmt.Sprintf("%sanull[aout]", mixLabels[0]))
	} else {
		duration := "longest"
		if req.TrimToVideoDuration {
			duration = "first" // duration=first uses the first input (video) length
		}
		// dropout_transition reduces popping when one input ends early.
		filterParts = append(filterParts, fmt.Sprintf(
			"%samix=inputs=%d:duration=%s:dropout_transition=2[aout]",
			strings.Join(mixLabels, ""), mixInputs, duration,
		))
	}

	filter := strings.Join(filterParts, ";")
	args = append(args,
		"-filter_complex", filter,
		"-map", "0:v:0",
		"-map", "[aout]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-b:a", "192k",
		"-shortest",
	)
	if req.TrimToVideoDuration {
		// `-shortest` already trims to the shortest input; the explicit flag
		// keeps intent obvious in logs.
	}
	args = append(args, outputPath)

	s.progress(job.ID, 25)

	if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
		// Re-encode video as a fallback if -c:v copy was rejected (e.g.,
		// HEVC-in-MOV → MP4 container with stricter codec compatibility).
		fallbackArgs := append([]string{}, args...)
		for i, v := range fallbackArgs {
			if v == "-c:v" && i+1 < len(fallbackArgs) {
				fallbackArgs[i+1] = "libx264"
			}
		}
		fallbackArgs = appendArgIfMissing(fallbackArgs, "-pix_fmt", "yuv420p")
		fallbackArgs = appendArgIfMissing(fallbackArgs, "-preset", "medium")
		fallbackArgs = appendArgIfMissing(fallbackArgs, "-crf", "20")
		if _, stderr2, err2 := runCommand(ctx, "ffmpeg", fallbackArgs...); err2 != nil {
			return fmt.Errorf("ffmpeg stitch failed: %w (copy: %s | reencode: %s)", err2, tail(stderr, 1000), tail(stderr2, 1000))
		}
	}

	s.progress(job.ID, 100)
	return nil
}

func (s *StitchAudioToVideoService) progress(jobID string, percent int) {
	if s.jobManager == nil {
		return
	}
	s.jobManager.SendProgressUpdate(jobID, percent)
}

// formatVolumeArg renders a float volume into FFmpeg-friendly syntax. We cap
// to two decimal places so the resulting filter string is predictable and
// reads cleanly in logs.
func formatVolumeArg(v float64) string {
	if v < 0 {
		v = 0
	}
	if v > 4 {
		v = 4
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// appendArgIfMissing inserts the (key, value) pair right before the trailing
// output path if the key isn't already in args.
func appendArgIfMissing(args []string, key, value string) []string {
	for _, a := range args {
		if a == key {
			return args
		}
	}
	// Insert before the last element (the output path).
	if len(args) == 0 {
		return []string{key, value}
	}
	tail := args[len(args)-1]
	return append(append(args[:len(args)-1], key, value), tail)
}
