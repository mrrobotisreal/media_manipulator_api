package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM119 (design DARKROOM_OUTPUTS_AND_CAPTIONS.md §3.5, §4): the CPU-bound
// Darkroom encoders and the server-wide gate in front of them, plus the
// embedded subtitle stream.

// --- VP9 ---------------------------------------------------------------------

// studioVP9Threads is libvpx's -threads for one export. The timing run on the
// 32-thread host (design §9, MM119): 32 threads was only 11-16 % faster than
// 16 for a lone encode, and two concurrent 16-thread encodes (the default
// gate) each kept 86 % of a lone encode's fps, so 16 lets the two default
// slots share the CPU without one encode taking every core.
const studioVP9Threads = "16"

// vp9EncodeArgs is libvpx-vp9 in constant-quality mode (-crf with -b:v 0),
// ladder low/medium/high = cpu-used 4/3/2, crf 40/34/31. The MM119 timing run
// on the server (design §9) moved low from the proposed cpu-used 5 to 4:
// with -deadline good and -row-mt 1, libvpx's speed 5 encoded at HALF the
// fps of speed 4 (1080p 25 vs 51 fps, 2160p 7 vs 17 fps) for the same size.
// 8-bit yuv420p like every other encoder.
func vp9EncodeArgs(quality string) []string {
	cpu, crf := "3", "34"
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "high":
		cpu, crf = "2", "31"
	case "low":
		cpu, crf = "4", "40"
	}
	return []string{
		"-c:v", "libvpx-vp9",
		"-row-mt", "1",
		"-deadline", "good",
		"-cpu-used", cpu,
		"-crf", crf,
		"-b:v", "0",
		"-tile-columns", "2",
		"-threads", studioVP9Threads,
		"-pix_fmt", "yuv420p",
	}
}

// StudioEncoderVP9 is the ffmpeg encoder id of a VP9 export.
const StudioEncoderVP9 = "libvpx-vp9"

// studioEncodeNeedsCPUGate reports whether an export's video encoder is
// CPU-bound and must take a slot from the CPU encode gate. NVENC H.264/HEVC
// and audio-only exports never wait (the x264/x265 fallbacks keep their
// pre-MM119 behaviour; design §3.5 leaves them a config lever).
func studioEncodeNeedsCPUGate(encoder string) bool {
	return encoder == StudioEncoderVP9
}

// --- CPU encode gate ---------------------------------------------------------

// StudioCPUEncodeGate is the server-wide semaphore in front of CPU-bound
// export encoders (STUDIO_CPU_ENCODE_CONCURRENCY). It is acquired INSIDE the
// render job, after the export was accepted and charged, so a full gate makes
// the export wait as Queued rather than refusing it; the wait is reported on
// the job's queueWaitMs and the render event's cpuQueueMs.
type StudioCPUEncodeGate struct {
	slots chan struct{}
}

// NewStudioCPUEncodeGate makes a gate with n slots (n < 1 is treated as 1).
func NewStudioCPUEncodeGate(n int) *StudioCPUEncodeGate {
	if n < 1 {
		n = 1
	}
	return &StudioCPUEncodeGate{slots: make(chan struct{}, n)}
}

// Capacity is the number of slots.
func (g *StudioCPUEncodeGate) Capacity() int { return cap(g.slots) }

// InUse is the number of slots taken (a snapshot).
func (g *StudioCPUEncodeGate) InUse() int { return len(g.slots) }

// Acquire takes a slot. When none is free it calls onQueued once (before
// blocking) and waits until a slot frees or ctx ends. waited is the time spent
// blocked (0 when a slot was free at once); queued reports whether it had to
// wait. release must be called exactly once on success.
func (g *StudioCPUEncodeGate) Acquire(ctx context.Context, onQueued func()) (release func(), waited time.Duration, queued bool, err error) {
	select {
	case g.slots <- struct{}{}:
		return g.releaser(), 0, false, nil
	default:
	}
	if onQueued != nil {
		onQueued()
	}
	start := time.Now()
	select {
	case g.slots <- struct{}{}:
		return g.releaser(), time.Since(start), true, nil
	case <-ctx.Done():
		return nil, time.Since(start), true, ctx.Err()
	}
}

func (g *StudioCPUEncodeGate) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { <-g.slots }) }
}

var (
	studioCPUGateOnce sync.Once
	studioCPUGate     *StudioCPUEncodeGate
)

// sharedStudioCPUEncodeGate is the process-wide gate, sized once from config.
func sharedStudioCPUEncodeGate(n int) *StudioCPUEncodeGate {
	studioCPUGateOnce.Do(func() { studioCPUGate = NewStudioCPUEncodeGate(n) })
	return studioCPUGate
}

// StudioJobStageQueued is the job's currentStage while a Darkroom export
// waits for a CPU encode slot (the Exports panel shows "Queued").
const StudioJobStageQueued = "queued"

// --- embedded subtitles ------------------------------------------------------

// StudioEmbeddedSubs is the subtitle stream an export muxes in (captions mode
// `embedded`): an SRT file the runner wrote, re-encoded by ffmpeg into the
// container's subtitle codec. Language is ISO 639-2/B ("" = untagged).
type StudioEmbeddedSubs struct {
	Path     string
	Language string
}

// BuildStudioSRT renders cues as SubRip text (1-based, sorted by start,
// empty cues dropped). Plain text only: embedded tracks carry no styling.
func BuildStudioSRT(cues []models.StudioCaptionCue) string {
	sorted := make([]models.StudioCaptionCue, 0, len(cues))
	for _, c := range cues {
		if strings.TrimSpace(c.Text) == "" || c.EndSeconds <= c.StartSeconds {
			continue
		}
		sorted = append(sorted, c)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartSeconds < sorted[j].StartSeconds })
	var b strings.Builder
	for i, c := range sorted {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtStamp(c.StartSeconds), srtStamp(c.EndSeconds), strings.TrimSpace(strings.ReplaceAll(c.Text, "\r", "")))
	}
	return b.String()
}

func srtStamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// iso6392B maps ISO 639-1 (what Whisper reports) to ISO 639-2/B, the code
// Matroska and QuickTime language tags use.
var iso6392B = map[string]string{
	"af": "afr", "ar": "ara", "bg": "bul", "bn": "ben", "ca": "cat", "cs": "cze", "cy": "wel", "da": "dan",
	"de": "ger", "el": "gre", "en": "eng", "es": "spa", "et": "est", "fa": "per", "fi": "fin", "fr": "fre",
	"ga": "gle", "he": "heb", "hi": "hin", "hr": "hrv", "hu": "hun", "hy": "arm", "id": "ind", "is": "ice",
	"it": "ita", "ja": "jpn", "ka": "geo", "kk": "kaz", "ko": "kor", "lt": "lit", "lv": "lav", "mk": "mac",
	"ms": "may", "mt": "mlt", "nl": "dut", "no": "nor", "nb": "nob", "nn": "nno", "pl": "pol", "pt": "por",
	"ro": "rum", "ru": "rus", "sk": "slo", "sl": "slv", "sq": "alb", "sr": "srp", "sv": "swe", "sw": "swa",
	"ta": "tam", "te": "tel", "th": "tha", "tl": "tgl", "tr": "tur", "uk": "ukr", "ur": "urd", "vi": "vie",
	"zh": "chi",
}

// StudioSubtitleLanguage converts a caption language ("de", "pt-BR",
// "zh_Hans", or already "ger") to the ISO 639-2/B tag of the subtitle stream;
// "" when unknown (ffmpeg then writes `und`).
func StudioSubtitleLanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if code, ok := iso6392B[l]; ok {
		return code
	}
	if len(l) == 3 {
		for _, b := range iso6392B {
			if b == l {
				return l
			}
		}
	}
	return ""
}

// subtitleStreamArgs maps the extra subtitle input (index idx) and encodes it
// in the container's subtitle codec, tagged with language and title.
func subtitleStreamArgs(o StudioOutput, subs *StudioEmbeddedSubs, idx int) []string {
	codec := o.SubtitleCodec()
	if subs == nil || codec == "" {
		return nil
	}
	args := []string{"-map", fmt.Sprintf("%d:s:0", idx), "-c:s", codec}
	if subs.Language != "" {
		args = append(args, "-metadata:s:s:0", "language="+subs.Language)
	}
	return append(args, "-metadata:s:s:0", "title=Captions")
}
