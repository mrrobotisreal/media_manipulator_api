package services

import (
	"context"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM120 golden args: every new row and quality. AV1 NVENC and SVT-AV1 per
// quality (MP4 with +faststart, WebM and MKV without), ProRes 422 MOV with
// 24-bit / 16-bit PCM and no +faststart, FLAC (MKV and .flac), 24-bit PCM
// (MOV, MKV, WAV). Lossless audio never gets a bitrate or a loudness change.
func TestStudioOutputMM120Args(t *testing.T) {
	nv := func(cq string) string {
		return "-c:v av1_nvenc -preset p5 -cq " + cq + " -b:v 0 -maxrate 120M -bufsize 240M -pix_fmt yuv420p"
	}
	svt := func(preset, crf string) string {
		return "-c:v libsvtav1 -preset " + preset + " -crf " + crf + " -pix_fmt yuv420p"
	}
	prores := "-c:v prores_ks -profile:v 2 -vendor apl0 -pix_fmt yuv422p10le"
	aac := " -c:a aac -b:a 192k"
	opus := " -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio"
	flac := " -c:a flac -sample_fmt s32 -ar 48000"
	pcm24 := " -c:a pcm_s24le -ar 48000"
	cases := []struct {
		name, quality string
		req           StudioOutputRequest
		enc           string
		tail          string
	}{
		{"mp4 av1 nvenc low", "low", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderAV1NVENC, nv("38") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 nvenc medium", "medium", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderAV1NVENC, nv("32") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 nvenc high", "high", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderAV1NVENC, nv("28") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 svt low", "low", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderSVTAV1, svt("10", "38") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 svt medium", "medium", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderSVTAV1, svt("8", "32") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 svt high", "high", StudioOutputRequest{Preset: "mp4_av1"}, StudioEncoderSVTAV1, svt("7", "28") + aac + " -t 5.000 -movflags +faststart /o.mp4"},
		{"mp4 av1 opus", "medium", StudioOutputRequest{Container: "mp4", VideoCodec: "av1", AudioCodec: "opus"}, StudioEncoderAV1NVENC, nv("32") + opus + " -t 5.000 -movflags +faststart /o.mp4"},
		{"webm av1 opus low", "low", StudioOutputRequest{Preset: "webm_av1_opus"}, StudioEncoderAV1NVENC, nv("38") + opus + " -t 5.000 /o.webm"},
		{"webm av1 opus medium", "medium", StudioOutputRequest{Preset: "webm_av1_opus"}, StudioEncoderAV1NVENC, nv("32") + opus + " -t 5.000 /o.webm"},
		{"webm av1 opus high", "high", StudioOutputRequest{Preset: "webm_av1_opus"}, StudioEncoderAV1NVENC, nv("28") + opus + " -t 5.000 /o.webm"},
		{"webm av1 vorbis svt", "high", StudioOutputRequest{Container: "webm", VideoCodec: "av1", AudioCodec: "vorbis"}, StudioEncoderSVTAV1, svt("7", "28") + " -c:a libvorbis -b:a 192k -t 5.000 /o.webm"},
		{"mkv av1 flac", "medium", StudioOutputRequest{Container: "mkv", VideoCodec: "av1", AudioCodec: "flac"}, StudioEncoderAV1NVENC, nv("32") + flac + " -t 5.000 /o.mkv"},
		{"mkv av1 aac mono", "low", StudioOutputRequest{Container: "mkv", VideoCodec: "av1", AudioChannels: 1}, StudioEncoderSVTAV1, svt("10", "38") + aac + " -ac 1 -t 5.000 /o.mkv"},
		{"mov prores (24-bit default)", "medium", StudioOutputRequest{Preset: "mov_prores"}, StudioEncoderProRes, prores + pcm24 + " -t 5.000 /o.mov"},
		{"mov prores ignores quality", "low", StudioOutputRequest{Preset: "mov_prores"}, StudioEncoderProRes, prores + pcm24 + " -t 5.000 /o.mov"},
		{"mov prores implicit audio", "high", StudioOutputRequest{Container: "mov", VideoCodec: "prores"}, StudioEncoderProRes, prores + pcm24 + " -t 5.000 /o.mov"},
		{"mov prores 16-bit", "high", StudioOutputRequest{Container: "mov", VideoCodec: "prores", AudioCodec: "pcm_s16le"}, StudioEncoderProRes, prores + " -c:a pcm_s16le -t 5.000 /o.mov"},
		{"mov prores 24-bit mono", "high", StudioOutputRequest{Preset: "mov_prores", AudioChannels: 1}, StudioEncoderProRes, prores + pcm24 + " -ac 1 -t 5.000 /o.mov"},
		{"mov hevc pcm24", "medium", StudioOutputRequest{Container: "mov", VideoCodec: "hevc", AudioCodec: "pcm_s24le"}, "hevc_nvenc",
			"-c:v hevc_nvenc -preset p4 -rc vbr -cq 28 -b:v 0 -pix_fmt yuv420p -tag:v hvc1" + pcm24 + " -t 5.000 -movflags +faststart /o.mov"},
		{"mkv hevc pcm24", "medium", StudioOutputRequest{Container: "mkv", VideoCodec: "hevc", AudioCodec: "pcm_s24le"}, "hevc_nvenc",
			"-c:v hevc_nvenc -preset p4 -rc vbr -cq 28 -b:v 0 -pix_fmt yuv420p" + pcm24 + " -t 5.000 /o.mkv"},
		{"audio flac", "medium", StudioOutputRequest{Preset: "audio_flac"}, "audio", "-vn" + flac + " -t 5.000 /o.flac"},
		{"audio flac mono", "medium", StudioOutputRequest{Container: "flac", AudioChannels: 1}, "audio", "-vn" + flac + " -ac 1 -t 5.000 /o.flac"},
		{"audio wav24", "medium", StudioOutputRequest{Preset: "audio_wav24"}, "audio", "-vn" + pcm24 + " -t 5.000 /o.wav"},
		{"audio wav16 unchanged", "medium", StudioOutputRequest{Preset: "audio_wav"}, "audio", "-vn -c:a pcm_s16le -t 5.000 /o.wav"},
	}
	for _, c := range cases {
		p := outputPlan(t, c.req)
		ext, _, _ := p.Output.File()
		args := joined(buildMultiTrackExportArgs(p, c.enc, c.quality, "/f.ttf", "/fd", "/o"+ext))
		if !strings.HasSuffix(args, c.tail) {
			t.Errorf("%s:\n got %s\nwant suffix %s", c.name, args, c.tail)
		}
		if strings.Contains(args, "loudnorm") {
			t.Errorf("%s: no loudness change without a loudness preset: %s", c.name, args)
		}
	}
}

// MM120 rows: ProRes narrows MOV's audio to PCM (24-bit first), AV1 never in
// MOV, ProRes never outside MOV, FLAC never in MP4/MOV/WebM, files and tokens.
func TestStudioOutputMM120Rows(t *testing.T) {
	for _, bad := range []StudioOutputRequest{
		{Container: "mov", VideoCodec: "prores", AudioCodec: "aac"},
		{Container: "mp4", VideoCodec: "prores"},
		{Container: "mkv", VideoCodec: "prores"},
		{Container: "webm", VideoCodec: "prores"},
		{Container: "mov", VideoCodec: "av1"},
		{Container: "mp4", AudioCodec: "flac"},
		{Container: "mov", AudioCodec: "flac"},
		{Container: "webm", VideoCodec: "av1", AudioCodec: "flac"},
		{Container: "mp4", AudioCodec: "pcm_s24le"},
		{Preset: "audio_flac", AudioBitrate: 128},
		{Preset: "audio_wav24", AudioBitrate: 128},
	} {
		if o, err := ResolveStudioOutput(bad); err == nil {
			t.Errorf("%+v resolved to %+v", bad, o)
		}
	}
	if got := strings.Join(StudioAudioCodecsFor("mov", "prores"), ","); got != "pcm_s24le,pcm_s16le" {
		t.Errorf("ProRes MOV audio = %s", got)
	}
	if got := strings.Join(StudioAudioCodecsFor("mov", "h264"), ","); got != "aac,pcm_s16le,pcm_s24le" {
		t.Errorf("H.264 MOV audio = %s", got)
	}
	for _, c := range []struct {
		req            StudioOutputRequest
		ext, mime      string
		tokens, label  string
		faststart, adv bool
	}{
		{StudioOutputRequest{Preset: "mp4_av1"}, ".mp4", "video/mp4", "mp4,av1,aac", "mp4_av1_aac", true, false},
		{StudioOutputRequest{Preset: "webm_av1_opus"}, ".webm", "video/webm", "webm,av1,opus", "webm_av1_opus", false, false},
		{StudioOutputRequest{Preset: "mov_prores"}, ".mov", "video/quicktime", "mov,prores,pcm_s24le", "mov_prores_pcm_s24le", false, false},
		{StudioOutputRequest{Container: "mov", VideoCodec: "prores", AudioCodec: "pcm_s16le"}, ".mov", "video/quicktime", "mov,prores,pcm_s16le", "mov_prores_pcm_s16le", false, true},
		{StudioOutputRequest{Preset: "audio_flac"}, ".flac", "audio/flac", "flac", "flac_none_flac", false, false},
		{StudioOutputRequest{Preset: "audio_wav24"}, ".wav", "audio/wav", "wav,pcm_s24le", "wav_none_pcm_s24le", false, false},
		{StudioOutputRequest{Container: "mkv", VideoCodec: "av1", AudioCodec: "flac"}, ".mkv", "video/x-matroska", "mkv,av1,flac", "mkv_av1_flac", false, true},
	} {
		o, err := ResolveStudioOutput(c.req)
		if err != nil {
			t.Fatalf("%+v: %v", c.req, err)
		}
		ext, mime, _ := o.File()
		if ext != c.ext || mime != c.mime || strings.Join(o.Tokens(), ",") != c.tokens || o.Label() != c.label || o.Faststart() != c.faststart || o.Advanced != c.adv {
			t.Errorf("%+v: %s %s %v %s faststart=%v adv=%v", c.req, ext, mime, o.Tokens(), o.Label(), o.Faststart(), o.Advanced)
		}
	}
	// The AV1 compatibility warning on every AV1 combination; 24-bit PCM in
	// MOV has no browser preview.
	for _, combo := range [][3]string{{"mp4", "av1", "aac"}, {"webm", "av1", "opus"}, {"mkv", "av1", "flac"}} {
		if w := strings.Join(StudioOutputWarnings(combo[0], combo[1], combo[2]), ","); !strings.Contains(w, StudioWarnAV1Playback) {
			t.Errorf("%v warnings = %s", combo, w)
		}
	}
	if w := strings.Join(StudioOutputWarnings("mov", "prores", "pcm_s24le"), ","); w != StudioWarnNoBrowserPreview {
		t.Errorf("ProRes MOV warnings = %s", w)
	}
}

// Probe selection: av1_nvenc -> libsvtav1 -> ErrStudioAV1Unavailable.
func TestPickAV1Encoder(t *testing.T) {
	if e, err := pickAV1Encoder(true, true); e != "av1_nvenc" || err != nil {
		t.Errorf("nvenc preferred: %q %v", e, err)
	}
	if e, err := pickAV1Encoder(true, false); e != "av1_nvenc" || err != nil {
		t.Errorf("nvenc without svt: %q %v", e, err)
	}
	if e, err := pickAV1Encoder(false, true); e != "libsvtav1" || err != nil {
		t.Errorf("svt fallback: %q %v", e, err)
	}
	e, err := pickAV1Encoder(false, false)
	if e != "" || err != ErrStudioAV1Unavailable {
		t.Errorf("neither: %q %v", e, err)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unsupported") || strings.Contains(msg, "nvenc") || strings.Contains(msg, "gpu") {
		t.Errorf("error must classify as unsupported (and not gpu): %v", err)
	}
}

func TestEncodersListHas(t *testing.T) {
	listing := " V....D libaom-av1           libaom AV1\n V..... libsvtav1            SVT-AV1\n V....D av1_nvenc            NVIDIA NVENC av1 encoder\n"
	if !encodersListHas(listing, "libsvtav1") || !encodersListHas(listing, "av1_nvenc") {
		t.Error("rows should be found")
	}
	if encodersListHas(listing, "libx265") || encodersListHas("", "libsvtav1") {
		t.Error("absent rows must be false")
	}
}

// RunExport picks the AV1 encoder from the probe, fails loudly without one
// (never a different codec), puts libsvtav1 and ProRes behind the CPU gate
// and keeps AV1 NVENC off it.
func TestRunExportAV1AndProResEncoders(t *testing.T) {
	studioAV1Once.Do(func() {})
	prevN, prevS := studioAV1NVENC, studioAV1SVT
	defer func() { studioAV1NVENC, studioAV1SVT = prevN, prevS }()
	av1, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "mp4_av1"})
	prores, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "mov_prores"})
	run := func(o StudioOutput) (StudioRenderStats, []string, error) {
		var got []string
		svc := &StudioExportService{cfg: &config.Config{}, jobManager: NewJobManager(), cpuGate: NewStudioCPUEncodeGate(1),
			run: func(_ context.Context, _ *JobManager, _ string, _ int, _ float64, args ...string) error {
				got = args
				return nil
			}}
		job := svc.jobManager.CreateJob(models.OriginalFileInfo{Name: "x"}, map[string]interface{}{"mode": "studio_export"})
		plan := depthPlan()
		plan.Output = &o
		s, err := svc.RunExport(context.Background(), job.ID, plan, "high", "/o")
		return s, got, err
	}
	studioAV1NVENC, studioAV1SVT = true, true
	if s, args, err := run(av1); err != nil || s.Encoder != "nvenc" || s.CPUEncode || !strings.Contains(joined(args), "-c:v av1_nvenc") {
		t.Errorf("nvenc: %+v %v", s, err)
	}
	studioAV1NVENC = false
	if s, args, err := run(av1); err != nil || s.Encoder != "svtav1" || !s.CPUEncode || !strings.Contains(joined(args), "-c:v libsvtav1") {
		t.Errorf("svt fallback: %+v %v", s, err)
	}
	studioAV1SVT = false
	if s, args, err := run(av1); err != ErrStudioAV1Unavailable || args != nil || s.VideoCodec != "av1" {
		t.Errorf("no AV1 encoder must fail before any ffmpeg: %+v %v %v", s, args, err)
	}
	if s, args, err := run(prores); err != nil || s.Encoder != "prores" || !s.CPUEncode || !strings.Contains(joined(args), "-c:v prores_ks -profile:v 2") {
		t.Errorf("prores: %+v %v", s, err)
	}
}

// The size estimate: ProRes ~147 Mb/s at 1080p30, scaled by pixels x fps,
// plus 24-bit stereo PCM; unpredictable codecs give no estimate.
func TestStudioOutputEstimateBytes(t *testing.T) {
	prores, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "mov_prores"})
	b, ok := StudioOutputEstimateBytes(prores, 1920, 1080, 30, 60)
	want := int64((147000 + 2304) * 1000 / 8 * 60)
	if !ok || b != want {
		t.Errorf("1080p30 60s = %d %v, want %d", b, ok, want)
	}
	b4k, _ := StudioOutputEstimateBytes(prores, 3840, 2160, 30, 60)
	if b4k < 4*b-int64(3*2304*1000/8*60) || b4k > 4*b {
		t.Errorf("2160p is ~4x 1080p: %d vs %d", b4k, b)
	}
	b60, _ := StudioOutputEstimateBytes(prores, 1920, 1080, 60, 60)
	if b60 < 2*b-int64(2304*1000/8*60) || b60 > 2*b {
		t.Errorf("60 fps is ~2x 30 fps: %d vs %d", b60, b)
	}
	mono, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "mov_prores", AudioChannels: 1})
	if bm, _ := StudioOutputEstimateBytes(mono, 1920, 1080, 30, 60); bm != b-int64(1152*1000/8*60) {
		t.Errorf("mono halves the PCM: %d", bm)
	}
	for _, req := range []StudioOutputRequest{{Preset: "mp4_av1"}, {Preset: "mp4_h264"}, {Preset: "audio_flac"}, {Preset: "webm_vp9_opus"}} {
		o, _ := ResolveStudioOutput(req)
		if _, ok := StudioOutputEstimateBytes(o, 1920, 1080, 30, 60); ok {
			t.Errorf("%s: constant-quality outputs have no estimate", req.Preset)
		}
	}
	wav24, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "audio_wav24"})
	if bw, ok := StudioOutputEstimateBytes(wav24, 0, 0, 0, 10); !ok || bw != 2304*1000/8*10 {
		t.Errorf("wav24 10s = %d %v", bw, ok)
	}
	if _, ok := StudioOutputEstimateBytes(prores, 1920, 1080, 30, 0); ok {
		t.Error("no duration, no estimate")
	}
}

func TestStudioOutputTooLarge(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		est        int64
		ok         bool
		max, free  int64
		refuse     bool
		wantReason string
	}{
		{4 * gb, true, 5 * gb, 100 * gb, false, ""},
		{6 * gb, true, 5 * gb, 100 * gb, true, "file_cap"},
		{4 * gb, true, 5 * gb, 5 * gb, true, "disk"}, // 4 + 2 headroom > 5
		{4 * gb, true, 5 * gb, -1, false, ""},        // unknown disk
		{4 * gb, true, 0, 100 * gb, false, ""},       // no cap
		{60 * gb, false, 5 * gb, 1 * gb, false, ""},  // no estimate, never refused
	} {
		refuse, reason := StudioOutputTooLarge(c.est, c.ok, c.max, c.free)
		if refuse != c.refuse || reason != c.wantReason {
			t.Errorf("%+v: %v %q", c, refuse, reason)
		}
	}
}
