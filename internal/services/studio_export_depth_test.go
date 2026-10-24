package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Part 20 export depth: golden-string coverage of the encoder / scale / format
// argument construction. ffmpeg is never run here (the laptop's is broken);
// the workstation verification list is in the ticket comment.

func depthPlan() StudioExportPlan {
	return StudioExportPlan{
		Inputs: inputs("/a.mp4"),
		Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1}},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Volume: 1}},
		Width:  1920, Height: 1080, FPS: 30, Duration: 5,
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestExportDepth_DefaultIsByteStable(t *testing.T) {
	// No Format / OutHeight: no scale statement, H.264 args, AAC, faststart.
	args := buildMultiTrackExportArgs(depthPlan(), "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4")
	if strings.Contains(filterComplex(t, args), "vscaled") {
		t.Errorf("default export must not scale: %s", filterComplex(t, args))
	}
	want := "-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a aac -b:a 192k -t 5.000 -movflags +faststart /o.mp4"
	if !strings.HasSuffix(joined(args), want) {
		t.Errorf("tail = %s\nwant suffix %s", joined(args), want)
	}
	// An explicit h264 format is identical to the empty one.
	p := depthPlan()
	p.Format = "h264"
	if joined(buildMultiTrackExportArgs(p, "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4")) != joined(args) {
		t.Error("format=h264 must equal the legacy request")
	}
}

func TestExportDepth_HEVCArgs(t *testing.T) {
	p := depthPlan()
	p.Format = "hevc"
	nv := joined(buildMultiTrackExportArgs(p, "hevc_nvenc", "high", "/f.ttf", "/fd", "/o.mp4"))
	for _, want := range []string{"-c:v hevc_nvenc", "-preset p5", "-cq 24", "-tag:v hvc1", "-c:a aac", "-movflags +faststart"} {
		if !strings.Contains(nv, want) {
			t.Errorf("hevc_nvenc args missing %q: %s", want, nv)
		}
	}
	if strings.Contains(nv, "libx264") || strings.Contains(nv, "h264_nvenc") {
		t.Errorf("hevc export must not carry an H.264 encoder: %s", nv)
	}
	sw := joined(buildMultiTrackExportArgs(p, "libx265", "low", "/f.ttf", "/fd", "/o.mp4"))
	for _, want := range []string{"-c:v libx265", "-preset veryfast", "-crf 32", "-tag:v hvc1", "-pix_fmt yuv420p"} {
		if !strings.Contains(sw, want) {
			t.Errorf("libx265 args missing %q: %s", want, sw)
		}
	}
}

func TestExportDepth_HEVCQualityLadder(t *testing.T) {
	cases := map[string]string{"high": "-cq 24", "medium": "-cq 28", "low": "-cq 33", "": "-cq 28"}
	for q, want := range cases {
		if got := joined(hevcEncodeArgs("hevc_nvenc", q)); !strings.Contains(got, want) {
			t.Errorf("hevc_nvenc quality %q = %s, want %s", q, got, want)
		}
	}
}

func TestPickHEVCEncoder(t *testing.T) {
	if e, err := pickHEVCEncoder(true, true); e != "hevc_nvenc" || err != nil {
		t.Errorf("nvenc preferred: %q %v", e, err)
	}
	if e, err := pickHEVCEncoder(false, true); e != "libx265" || err != nil {
		t.Errorf("x265 fallback: %q %v", e, err)
	}
	e, err := pickHEVCEncoder(false, false)
	if e != "" || err != ErrStudioHEVCUnavailable {
		t.Errorf("neither: %q %v", e, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
		t.Errorf("error must classify as unsupported: %v", err)
	}
}

func TestEncodersListHasX265(t *testing.T) {
	listing := " V....D libx264              libx264 H.264\n V....D libx265              libx265 H.265 / HEVC\n V....D hevc_nvenc           NVIDIA NVENC hevc encoder\n"
	if !encodersListHasX265(listing) {
		t.Error("libx265 row should be found")
	}
	if encodersListHasX265(" V....D libx264 libx264 H.264\n V....D hevc_nvenc NVENC\n") {
		t.Error("no libx265 row should be false")
	}
}

func TestExportDepth_ResolutionScale(t *testing.T) {
	p := depthPlan()
	p.OutHeight = 720
	fc := filterComplex(t, buildMultiTrackExportArgs(p, "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4"))
	if !strings.Contains(fc, "[vout]scale=1280:720:flags=lanczos,setsar=1[vscaled]") {
		t.Errorf("expected 720p scale of [vout]: %s", fc)
	}
	args := joined(buildMultiTrackExportArgs(p, "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4"))
	if !strings.Contains(args, "-map [vscaled]") || strings.Contains(args, "-map [vout]") {
		t.Errorf("video map must follow the scaled label: %s", args)
	}
}

func TestExportDepth_ScaleAfterCaptionBurn(t *testing.T) {
	p := depthPlan()
	p.OutHeight = 720
	p.CaptionsASSPath = "/job/captions.ass"
	fc := filterComplex(t, buildMultiTrackExportArgs(p, "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4"))
	burn := strings.Index(fc, "subtitles=filename")
	scale := strings.Index(fc, "scale=1280:720")
	if burn < 0 || scale < 0 || scale < burn {
		t.Errorf("captions must burn at canvas size BEFORE the output scale: %s", fc)
	}
}

func TestExportDepth_ScaleSkippedWhenNoOp(t *testing.T) {
	for _, h := range []int{0, 1080} {
		p := depthPlan()
		p.OutHeight = h
		if strings.Contains(filterComplex(t, buildMultiTrackExportArgs(p, "h264_nvenc", "high", "/f.ttf", "/fd", "/o.mp4")), "vscaled") {
			t.Errorf("OutHeight %d on a 1080 canvas must not scale", h)
		}
	}
}

func TestOutputDimensions(t *testing.T) {
	cases := []struct{ w, h, out, ww, hh int }{
		{1920, 1080, 0, 1920, 1080},
		{1920, 1080, 720, 1280, 720},
		{1920, 1080, 2160, 3840, 2160},
		{1080, 1920, 720, 406, 720},    // 9:16 -> 405 rounds to even 406
		{1080, 1080, 1440, 1440, 1440}, // 1:1
		{1080, 1350, 1080, 864, 1080},  // 4:5 canvas scaled to 1080 tall
		{1280, 720, 720, 1280, 720},
	}
	for _, c := range cases {
		w, h := OutputDimensions(c.w, c.h, c.out)
		if w != c.ww || h != c.hh {
			t.Errorf("OutputDimensions(%d,%d,%d) = %dx%d, want %dx%d", c.w, c.h, c.out, w, h, c.ww, c.hh)
		}
		if w%2 != 0 {
			t.Errorf("width %d must be even", w)
		}
	}
}

func TestExportDepth_AudioOnlyMP3AndWAV(t *testing.T) {
	for format, want := range map[string]string{
		"mp3": "-vn -c:a libmp3lame -b:a 192k -t 5.000 /o.mp3",
		"wav": "-vn -c:a pcm_s16le -t 5.000 /o.wav",
	} {
		p := depthPlan()
		p.Format = format
		p.OutHeight = 720 // must be ignored for audio
		args := buildMultiTrackExportArgs(p, "audio", "high", "/f.ttf", "/fd", "/o."+format)
		s := joined(args)
		if !strings.HasSuffix(s, want) {
			t.Errorf("%s tail = %s, want suffix %s", format, s, want)
		}
		fc := filterComplex(t, args)
		if strings.Contains(fc, "color=c=black") || strings.Contains(fc, "vscaled") || strings.Contains(fc, "[vout]") {
			t.Errorf("%s must skip the video graph entirely: %s", format, fc)
		}
		if strings.Contains(s, "-c:v") || strings.Contains(s, "-movflags") || strings.Contains(s, "aac") {
			t.Errorf("%s carries video/mp4 args: %s", format, s)
		}
		if !strings.Contains(s, "-map [aout]") {
			t.Errorf("%s missing audio map: %s", format, s)
		}
	}
}

func TestExportDepth_AudioOnlyKeepsMixerChain(t *testing.T) {
	p := depthPlan()
	p.Format = "mp3"
	p.Loudness = "streaming"
	fc := filterComplex(t, buildMultiTrackExportArgs(p, "audio", "high", "/f.ttf", "/fd", "/o.mp3"))
	if !strings.Contains(fc, "loudnorm") {
		t.Errorf("audio export keeps the loudness stage: %s", fc)
	}
}

func TestExportDepth_AudioOnlyKeyframedPanRidesAsendcmd(t *testing.T) {
	p := depthPlan()
	p.Format = "wav"
	p.SendCmdPath = "/job/keyframes.cmd"
	fc := filterComplex(t, buildMultiTrackExportArgs(p, "audio", "high", "/f.ttf", "/fd", "/o.wav"))
	if !strings.Contains(fc, "asendcmd=f='/job/keyframes.cmd'") || strings.Contains(fc, ",sendcmd=") {
		t.Errorf("sidecar must ride asendcmd for an audio export: %s", fc)
	}
}

func TestArgsHaveAudioMap(t *testing.T) {
	p := depthPlan()
	p.Format = "mp3"
	if !argsHaveAudioMap(buildMultiTrackExportArgs(p, "audio", "", "", "", "/o.mp3")) {
		t.Error("expected an audio map")
	}
	p.Audio = nil
	if argsHaveAudioMap(buildMultiTrackExportArgs(p, "audio", "", "", "", "/o.mp3")) {
		t.Error("silent plan must have no audio map")
	}
}

func TestExportDepth_FrameArgs(t *testing.T) {
	p := depthPlan()
	p.Format = "mp3" // must be ignored by the frame path
	p.OutHeight = 0
	args := buildFrameExportArgs(p, 2.5, "/f.ttf", "/fd", "/frame.png")
	s := joined(args)
	for _, want := range []string{"-ss 2.500", "-frames:v 1", "-an", "-map [vout]"} {
		if !strings.Contains(s, want) {
			t.Errorf("frame args missing %q: %s", want, s)
		}
	}
	if !strings.HasSuffix(s, "-an /frame.png") {
		t.Errorf("tail: %s", s)
	}
	fc := filterComplex(t, args)
	if strings.Contains(fc, "[aout]") || strings.Contains(fc, "amix") || strings.Contains(fc, "adelay") {
		t.Errorf("frame render must carry no audio graph: %s", fc)
	}
	if !strings.Contains(fc, "color=c=black:s=1920x1080:r=30:d=3.500") {
		t.Errorf("canvas must cover t (+1s): %s", fc)
	}
	// Negative t clamps to 0.
	if !strings.Contains(joined(buildFrameExportArgs(p, -4, "", "", "/f.png")), "-ss 0.000") {
		t.Error("negative t must clamp to 0")
	}
}

func TestExportDepth_FrameBlankWhenNothingVisible(t *testing.T) {
	p := depthPlan()
	p.Video = nil
	fc := filterComplex(t, buildFrameExportArgs(p, 1, "", "", "/f.png"))
	if !strings.Contains(fc, "color=c=black") || !strings.Contains(fc, "[vbase]") {
		t.Errorf("black canvas expected: %s", fc)
	}
}

func clipRef(track models.StudioTrackKind, start, in, out float64, hasAudio bool) StudioClipRef {
	return StudioClipRef{
		Clip:      models.StudioClip{TimelineStart: start, SourceIn: in, SourceOut: out},
		Asset:     &models.StudioAsset{HasAudio: hasAudio},
		TrackKind: track,
	}
}

func TestFrameRefs(t *testing.T) {
	refs := []StudioClipRef{
		clipRef(models.StudioTrackKindVideo, 0, 0, 2, true),
		clipRef(models.StudioTrackKindVideo, 2, 0, 2, true),
		clipRef(models.StudioTrackKindAudio, 0, 0, 10, true),
	}
	if got := FrameRefs(refs, 1); len(got) != 1 || got[0].Clip.TimelineStart != 0 {
		t.Errorf("t=1 want the first video clip only: %v", got)
	}
	if got := FrameRefs(refs, 2); len(got) != 1 || got[0].Clip.TimelineStart != 2 {
		t.Errorf("t=2 is the second clip's first frame: %v", got)
	}
	if got := FrameRefs(refs, 9); len(got) != 0 {
		t.Errorf("past every video clip: %v", got)
	}
}

func TestAudioOnlyRefs(t *testing.T) {
	silent := clipRef(models.StudioTrackKindVideo, 0, 0, 2, false)
	muted := clipRef(models.StudioTrackKindAudio, 0, 0, 2, true)
	muted.TrackMuted = true
	loud := clipRef(models.StudioTrackKindVideo, 0, 0, 2, true)
	title := clipRef(models.StudioTrackKindVideo, 0, 0, 2, false)
	title.Clip.Title = &models.StudioTitle{}
	got := AudioOnlyRefs([]StudioClipRef{silent, muted, loud, title})
	if len(got) != 1 || !got[0].Asset.HasAudio || got[0].TrackMuted {
		t.Errorf("only the audible clip survives: %v", got)
	}
}

func TestEncoderAnalyticsNameDepth(t *testing.T) {
	for in, want := range map[string]string{
		"h264_nvenc": "nvenc", "hevc_nvenc": "nvenc", "libx264": "x264", "libx265": "x265", "audio": "audio",
	} {
		if got := encoderAnalyticsName(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestNormalizeExportFormat(t *testing.T) {
	for in, want := range map[string]string{"": "h264", "H264": "h264", "HEVC": "hevc", "mp3": "mp3", "wav": "wav", "gif": "h264"} {
		if got := NormalizeExportFormat(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if IsAudioFormat("hevc") || !IsAudioFormat("MP3") || !IsAudioFormat("wav") {
		t.Error("IsAudioFormat")
	}
}

func TestStudioExportsSQL(t *testing.T) {
	for _, want := range []string{"INSERT INTO studio_exports", "RETURNING " + studioExportColumns} {
		if !strings.Contains(studioInsertExportSQL, want) {
			t.Errorf("insert SQL missing %q", want)
		}
	}
	// Settle may only move a running row (no reopening, no overwrite).
	if !strings.Contains(studioSettleExportSQL, "AND status = 'running'") {
		t.Error("settle must be guarded on status = 'running'")
	}
	// Reads are always project-scoped, so an id can't cross projects.
	if !strings.Contains(studioListExportsSQL, "WHERE project_id = $1") ||
		!strings.Contains(studioGetExportSQL, "WHERE project_id = $1 AND id = $2") {
		t.Error("export reads must be scoped by project_id")
	}
	if !strings.Contains(studioListExportsSQL, "ORDER BY created_at DESC") {
		t.Error("list must be newest first")
	}
}
