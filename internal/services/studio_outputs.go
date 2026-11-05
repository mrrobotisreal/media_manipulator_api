package services

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Darkroom output table (MM118, ADR-0019, design DARKROOM_OUTPUTS_AND_CAPTIONS.md
// §3). THE ONLY PLACE OUTPUT COMBINATIONS LIVE: which containers exist, which
// video and audio codecs each may carry, the file extension, MIME and analytics
// media kind, the warnings a combination shows, the audio bitrates per codec,
// the closed preset list, the legacy `format` aliases and the default minimum
// tier per output token. Request validation, the history label, the ffmpeg
// argument builder, the tier gate's token list, Medi's export suggestions and
// GET /api/studio/export-options all read it. The UI never keeps its own copy:
// it reads the options route, with testdata/studio_outputs.json (written by
// TestStudioOutputTableFixture) committed byte for byte as its offline fallback.
//
// Adding a format (MM119, MM120) is a row here plus an encoder function.

// Output tokens: container keys and codec keys share one vocabulary (the tier
// FormatSet, analytics, the request). "mp3" is both the audio-only container and
// the audio codec; "none" is the video codec of an audio-only container and is
// never a gated token.
const (
	StudioContainerMP4 = "mp4"
	StudioContainerMOV = "mov"
	StudioContainerMKV = "mkv"
	StudioContainerMP3 = "mp3"
	StudioContainerWAV = "wav"

	StudioVideoH264 = "h264"
	StudioVideoHEVC = "hevc"
	StudioVideoNone = "none"

	StudioAudioAAC  = "aac"
	StudioAudioPCM  = "pcm_s16le"
	StudioAudioMP3  = "mp3"
	StudioAudioNone = "none"
)

// Warnings a combination carries (closed list; the UI translates them).
const (
	StudioWarnAppleIncompatible = "apple_incompatible" // QuickTime / Photos / iOS will not open it
	StudioWarnNoBrowserPreview  = "no_browser_preview" // a browser cannot play it inline
)

// Default stereo; mono is an Advanced choice.
const studioDefaultChannels = 2

// StudioOutputContainer is one container row.
type StudioOutputContainer struct {
	Key         string   `json:"key"`
	Ext         string   `json:"ext"`
	MIME        string   `json:"mime"`
	MediaKind   string   `json:"mediaKind"` // analytics media kind: video | audio
	AudioOnly   bool     `json:"audioOnly"`
	VideoCodecs []string `json:"videoCodecs"`
	AudioCodecs []string `json:"audioCodecs"`
	// Faststart moves the MP4/MOV index to the front (-movflags +faststart).
	Faststart bool `json:"faststart"`
}

// StudioOutputCodec is one codec row.
type StudioOutputCodec struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // video | audio
	// Bitrates is the closed list (kbps) the Advanced picker offers; empty =
	// the codec has no bitrate (PCM). DefaultBitrate is what a preset uses.
	Bitrates       []int `json:"bitrates"`
	DefaultBitrate int   `json:"defaultBitrate"`
}

// StudioOutputWarning attaches a warning to every combination matching its
// non-empty fields.
type StudioOutputWarning struct {
	Warning    string `json:"warning"`
	Container  string `json:"container,omitempty"`
	VideoCodec string `json:"videoCodec,omitempty"`
	AudioCodec string `json:"audioCodec,omitempty"`
}

// StudioOutputPreset is one named row everyone sees (each still gated by its
// tokens' tier).
type StudioOutputPreset struct {
	Key        string `json:"key"`
	Container  string `json:"container"`
	VideoCodec string `json:"videoCodec"`
	AudioCodec string `json:"audioCodec"`
}

// StudioOutputTable is the whole table, as served and as committed.
type StudioOutputTable struct {
	Containers []StudioOutputContainer `json:"containers"`
	Codecs     []StudioOutputCodec     `json:"codecs"`
	Warnings   []StudioOutputWarning   `json:"warnings"`
	Presets    []StudioOutputPreset    `json:"presets"`
	// LegacyFormats maps the pre-MM118 `format` values onto their preset.
	LegacyFormats map[string]string `json:"legacyFormats"`
	// Formats is the closed derived-label vocabulary (history, analytics).
	Formats []string `json:"formats"`
	// Tokens are the gated output tokens in canonical order; MinTier is the
	// DEFAULT ladder (TIER_*_STUDIO_FORMATS may move a token; the options route
	// serves the live sets beside this).
	Tokens  []string          `json:"tokens"`
	MinTier map[string]string `json:"minTier"`
	// AdvancedMinTier is the default plan of the Advanced picker.
	AdvancedMinTier string `json:"advancedMinTier"`
	DefaultChannels int    `json:"defaultChannels"`
	Channels        []int  `json:"channels"`
}

var studioOutputTable = StudioOutputTable{
	Containers: []StudioOutputContainer{
		{Key: StudioContainerMP4, Ext: ".mp4", MIME: "video/mp4", MediaKind: "video",
			VideoCodecs: []string{StudioVideoH264, StudioVideoHEVC}, AudioCodecs: []string{StudioAudioAAC}, Faststart: true},
		{Key: StudioContainerMOV, Ext: ".mov", MIME: "video/quicktime", MediaKind: "video",
			VideoCodecs: []string{StudioVideoH264, StudioVideoHEVC}, AudioCodecs: []string{StudioAudioAAC, StudioAudioPCM}, Faststart: true},
		{Key: StudioContainerMKV, Ext: ".mkv", MIME: "video/x-matroska", MediaKind: "video",
			VideoCodecs: []string{StudioVideoH264, StudioVideoHEVC}, AudioCodecs: []string{StudioAudioAAC, StudioAudioPCM, StudioAudioMP3}},
		{Key: StudioContainerMP3, Ext: ".mp3", MIME: "audio/mpeg", MediaKind: "audio", AudioOnly: true,
			VideoCodecs: []string{StudioVideoNone}, AudioCodecs: []string{StudioAudioMP3}},
		{Key: StudioContainerWAV, Ext: ".wav", MIME: "audio/wav", MediaKind: "audio", AudioOnly: true,
			VideoCodecs: []string{StudioVideoNone}, AudioCodecs: []string{StudioAudioPCM}},
	},
	Codecs: []StudioOutputCodec{
		{Key: StudioVideoH264, Kind: "video", Bitrates: []int{}},
		{Key: StudioVideoHEVC, Kind: "video", Bitrates: []int{}},
		{Key: StudioAudioAAC, Kind: "audio", Bitrates: []int{128, 192, 256, 320}, DefaultBitrate: 192},
		{Key: StudioAudioPCM, Kind: "audio", Bitrates: []int{}},
		{Key: StudioAudioMP3, Kind: "audio", Bitrates: []int{128, 192, 256, 320}, DefaultBitrate: 192},
	},
	Warnings: []StudioOutputWarning{
		{Warning: StudioWarnAppleIncompatible, Container: StudioContainerMKV},
		{Warning: StudioWarnNoBrowserPreview, Container: StudioContainerMKV},
		// Chrome and Firefox do not decode PCM inside MOV.
		{Warning: StudioWarnNoBrowserPreview, Container: StudioContainerMOV, AudioCodec: StudioAudioPCM},
	},
	Presets: []StudioOutputPreset{
		{Key: "mp4_h264", Container: StudioContainerMP4, VideoCodec: StudioVideoH264, AudioCodec: StudioAudioAAC},
		{Key: "mp4_hevc", Container: StudioContainerMP4, VideoCodec: StudioVideoHEVC, AudioCodec: StudioAudioAAC},
		{Key: "mov_h264", Container: StudioContainerMOV, VideoCodec: StudioVideoH264, AudioCodec: StudioAudioAAC},
		{Key: "mov_hevc", Container: StudioContainerMOV, VideoCodec: StudioVideoHEVC, AudioCodec: StudioAudioAAC},
		{Key: "mkv_hevc_aac", Container: StudioContainerMKV, VideoCodec: StudioVideoHEVC, AudioCodec: StudioAudioAAC},
		{Key: "audio_mp3", Container: StudioContainerMP3, VideoCodec: StudioVideoNone, AudioCodec: StudioAudioMP3},
		{Key: "audio_wav", Container: StudioContainerWAV, VideoCodec: StudioVideoNone, AudioCodec: StudioAudioPCM},
	},
	LegacyFormats: map[string]string{
		StudioFormatH264: "mp4_h264",
		StudioFormatHEVC: "mp4_hevc",
		StudioFormatMP3:  "audio_mp3",
		StudioFormatWAV:  "audio_wav",
	},
	Tokens: []string{
		StudioContainerMP4, StudioContainerMOV, StudioContainerMKV, StudioContainerMP3, StudioContainerWAV,
		StudioVideoH264, StudioVideoHEVC, StudioAudioAAC, StudioAudioPCM,
	},
	MinTier: map[string]string{
		StudioContainerMP4: "anonymous", StudioContainerMP3: "anonymous", StudioContainerWAV: "anonymous",
		StudioVideoH264: "anonymous", StudioAudioAAC: "anonymous", StudioAudioPCM: "anonymous",
		StudioVideoHEVC:    "free",
		StudioContainerMOV: "premium", StudioContainerMKV: "premium",
	},
	AdvancedMinTier: "premium",
	DefaultChannels: studioDefaultChannels,
	Channels:        []int{2, 1},
}

func init() {
	studioOutputTable.Formats = studioDerivedFormats()
}

// StudioOutputs returns the table (a copy of the top-level struct; treat the
// slices as read-only).
func StudioOutputs() StudioOutputTable { return studioOutputTable }

// StudioOutputTableJSON is the committed / served static table, indented and
// newline-terminated so the fixture diff stays readable.
func StudioOutputTableJSON() []byte {
	b, err := json.MarshalIndent(studioOutputTable, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func studioContainer(key string) (StudioOutputContainer, bool) {
	for _, c := range studioOutputTable.Containers {
		if c.Key == key {
			return c, true
		}
	}
	return StudioOutputContainer{}, false
}

func studioCodec(key string) (StudioOutputCodec, bool) {
	for _, c := range studioOutputTable.Codecs {
		if c.Key == key {
			return c, true
		}
	}
	return StudioOutputCodec{}, false
}

// StudioOutputPresetByKey finds a preset.
func StudioOutputPresetByKey(key string) (StudioOutputPreset, bool) {
	for _, p := range studioOutputTable.Presets {
		if p.Key == key {
			return p, true
		}
	}
	return StudioOutputPreset{}, false
}

func studioPresetFor(container, video, audio string) (StudioOutputPreset, bool) {
	for _, p := range studioOutputTable.Presets {
		if p.Container == container && p.VideoCodec == video && p.AudioCodec == audio {
			return p, true
		}
	}
	return StudioOutputPreset{}, false
}

// StudioOutputLabel is the derived history / analytics label: the legacy name
// for the four combinations `format` always meant, else
// <container>_<video>_<audio>. Bitrate and channels are not part of it (the
// label is a closed vocabulary).
func StudioOutputLabel(container, video, audio string) string {
	if p, ok := studioPresetFor(container, video, audio); ok {
		for legacy, key := range studioOutputTable.LegacyFormats {
			if key == p.Key {
				return legacy
			}
		}
	}
	return container + "_" + video + "_" + audio
}

func studioDerivedFormats() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, l := range []string{StudioFormatH264, StudioFormatHEVC, StudioFormatMP3, StudioFormatWAV} {
		add(l)
	}
	for _, c := range studioOutputTable.Containers {
		for _, v := range c.VideoCodecs {
			for _, a := range c.AudioCodecs {
				add(StudioOutputLabel(c.Key, v, a))
			}
		}
	}
	return out
}

// StudioOutputWarnings lists the warnings of one combination, in table order.
func StudioOutputWarnings(container, video, audio string) []string {
	out := []string{}
	for _, w := range studioOutputTable.Warnings {
		if (w.Container == "" || w.Container == container) && (w.VideoCodec == "" || w.VideoCodec == video) && (w.AudioCodec == "" || w.AudioCodec == audio) {
			out = append(out, w.Warning)
		}
	}
	return out
}

// StudioOutput is a fully resolved, valid export output.
type StudioOutput struct {
	Container     string
	VideoCodec    string
	AudioCodec    string
	AudioBitrate  int // kbps; 0 when the codec has none (PCM)
	AudioChannels int // 1 | 2
	// Preset is the matching preset key ("" = a combination no preset names).
	Preset string
	// Advanced is true when the output needs the Advanced picker: a
	// combination no preset names, or a non-default bitrate or channel count.
	Advanced bool
}

// Label is the derived history / analytics label.
func (o StudioOutput) Label() string {
	return StudioOutputLabel(o.Container, o.VideoCodec, o.AudioCodec)
}

// AudioOnly reports an audio-only container (no video graph at all).
func (o StudioOutput) AudioOnly() bool {
	c, ok := studioContainer(o.Container)
	return ok && c.AudioOnly
}

// File is the container's extension (".mov"), MIME and analytics media kind.
func (o StudioOutput) File() (ext, mime, mediaKind string) {
	c, ok := studioContainer(o.Container)
	if !ok {
		c, _ = studioContainer(StudioContainerMP4)
	}
	return c.Ext, c.MIME, c.MediaKind
}

// Faststart reports whether the container takes -movflags +faststart.
func (o StudioOutput) Faststart() bool {
	c, ok := studioContainer(o.Container)
	return ok && c.Faststart
}

// Tokens are the gated output tokens of this output (container, video codec
// unless none, audio codec), in that order.
func (o StudioOutput) Tokens() []string {
	out := []string{o.Container}
	if o.VideoCodec != StudioVideoNone && o.VideoCodec != "" {
		out = append(out, o.VideoCodec)
	}
	if o.AudioCodec != o.Container && o.AudioCodec != "" {
		out = append(out, o.AudioCodec)
	}
	return out
}

// Warnings are this output's table warnings.
func (o StudioOutput) Warnings() []string {
	return StudioOutputWarnings(o.Container, o.VideoCodec, o.AudioCodec)
}

// StudioOutputRequest is the output half of an export request, as sent.
type StudioOutputRequest struct {
	Format        string // legacy alias: '' | h264 | hevc | mp3 | wav
	Preset        string // output preset key
	Container     string
	VideoCodec    string
	AudioCodec    string
	AudioBitrate  int
	AudioChannels int
}

// StudioOutputError is a client-facing validation failure (400).
type StudioOutputError struct{ Msg string }

func (e *StudioOutputError) Error() string { return e.Msg }

func outputErr(format string, a ...any) error {
	return &StudioOutputError{Msg: fmt.Sprintf(format, a...)}
}

func normToken(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ResolveStudioOutput validates an output request against the table.
//
//   - `format` is an alias (legacy values only); `preset` names a preset. Both
//     set the combination; explicit container/videoCodec/audioCodec fields fill
//     or confirm it, and an explicit field that contradicts the alias or the
//     preset is an error (never a silent pick).
//   - With neither, the container defaults to mp4 and each codec to the
//     container's first one, so the empty request is exactly H.264 + AAC MP4.
//   - audioBitrate 0 = the codec's default; otherwise one of its closed list
//     (a codec without bitrates refuses any). audioChannels 0 = stereo; 1 | 2.
func ResolveStudioOutput(r StudioOutputRequest) (StudioOutput, error) {
	format, preset := normToken(r.Format), normToken(r.Preset)
	container, video, audio := normToken(r.Container), normToken(r.VideoCodec), normToken(r.AudioCodec)

	var base *StudioOutputPreset
	if format != "" {
		key, ok := studioOutputTable.LegacyFormats[format]
		if !ok {
			return StudioOutput{}, outputErr("Unsupported export format")
		}
		p, _ := StudioOutputPresetByKey(key)
		base = &p
	}
	if preset != "" {
		p, ok := StudioOutputPresetByKey(preset)
		if !ok {
			return StudioOutput{}, outputErr("Unsupported export preset")
		}
		if base != nil && base.Key != p.Key {
			return StudioOutput{}, outputErr("format %q conflicts with preset %q", format, preset)
		}
		base = &p
	}
	if base != nil {
		for _, f := range []struct{ name, got, want string }{
			{"container", container, base.Container},
			{"videoCodec", video, base.VideoCodec},
			{"audioCodec", audio, base.AudioCodec},
		} {
			if f.got != "" && f.got != f.want {
				return StudioOutput{}, outputErr("%s %q conflicts with the requested format", f.name, f.got)
			}
		}
		container, video, audio = base.Container, base.VideoCodec, base.AudioCodec
	}
	if container == "" {
		container = StudioContainerMP4
	}
	c, ok := studioContainer(container)
	if !ok {
		return StudioOutput{}, outputErr("Unsupported export container")
	}
	if video == "" {
		video = c.VideoCodecs[0]
	}
	if audio == "" {
		audio = c.AudioCodecs[0]
	}
	if !containsStr(c.VideoCodecs, video) {
		if _, known := studioCodec(video); !known && video != StudioVideoNone {
			return StudioOutput{}, outputErr("Unsupported video codec")
		}
		return StudioOutput{}, outputErr("%s cannot carry %s video", strings.ToUpper(container), video)
	}
	if !containsStr(c.AudioCodecs, audio) {
		if _, known := studioCodec(audio); !known {
			return StudioOutput{}, outputErr("Unsupported audio codec")
		}
		return StudioOutput{}, outputErr("%s cannot carry %s audio", strings.ToUpper(container), audio)
	}
	codec, _ := studioCodec(audio)
	bitrate := r.AudioBitrate
	switch {
	case bitrate < 0:
		return StudioOutput{}, outputErr("Unsupported audio bitrate")
	case bitrate == 0:
		bitrate = codec.DefaultBitrate
	case len(codec.Bitrates) == 0:
		return StudioOutput{}, outputErr("%s audio has no bitrate setting", audio)
	case !containsInt(codec.Bitrates, bitrate):
		return StudioOutput{}, outputErr("Unsupported audio bitrate")
	}
	channels := r.AudioChannels
	if channels == 0 {
		channels = studioDefaultChannels
	}
	if channels != 1 && channels != 2 {
		return StudioOutput{}, outputErr("Unsupported audio channels")
	}
	out := StudioOutput{Container: container, VideoCodec: video, AudioCodec: audio, AudioBitrate: bitrate, AudioChannels: channels}
	if p, ok := studioPresetFor(container, video, audio); ok {
		out.Preset = p.Key
	}
	out.Advanced = out.Preset == "" || bitrate != codec.DefaultBitrate || channels != studioDefaultChannels
	return out, nil
}

// LegacyStudioOutput resolves a stored or plan-level legacy format ("" and
// anything unknown = H.264 MP4, the pre-MM118 NormalizeExportFormat rule).
func LegacyStudioOutput(format string) StudioOutput {
	out, err := ResolveStudioOutput(StudioOutputRequest{Format: NormalizeExportFormat(format)})
	if err != nil {
		out, _ = ResolveStudioOutput(StudioOutputRequest{})
	}
	return out
}

// StoredStudioOutput rebuilds the output of a studio_exports row. Rows from
// before the MM118 migration (or written by a pre-MM118 binary after it) have
// no container: their legacy `format` says everything.
func StoredStudioOutput(format, container, video, audio string, bitrate, channels *int) (StudioOutput, error) {
	if strings.TrimSpace(container) == "" {
		return LegacyStudioOutput(format), nil
	}
	r := StudioOutputRequest{Container: container, VideoCodec: video, AudioCodec: audio}
	if bitrate != nil {
		r.AudioBitrate = *bitrate
	}
	if channels != nil {
		r.AudioChannels = *channels
	}
	return ResolveStudioOutput(r)
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// audioEncodeArgs is the audio codec half of the output args (without -vn).
// The default stereo output adds nothing (the mix is already stereo), so the
// legacy rows stay byte-identical; mono downmixes with -ac 1.
func audioEncodeArgs(o StudioOutput) []string {
	var args []string
	switch o.AudioCodec {
	case StudioAudioPCM:
		args = []string{"-c:a", "pcm_s16le"}
	case StudioAudioMP3:
		args = []string{"-c:a", "libmp3lame", "-b:a", fmt.Sprintf("%dk", o.AudioBitrate)}
	default: // aac
		args = []string{"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", o.AudioBitrate)}
	}
	if o.AudioChannels == 1 {
		args = append(args, "-ac", "1")
	}
	return args
}

// videoEncodeArgs is the video codec half for a container. MKV drops the
// MP4-family hvc1 tag (Matroska names HEVC by its CodecID).
func videoEncodeArgs(o StudioOutput, encoder, quality string) []string {
	if o.VideoCodec != StudioVideoHEVC {
		return h264EncodeArgs(encoder, quality)
	}
	args := hevcEncodeArgs(encoder, quality)
	if o.Container != StudioContainerMKV {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-tag:v" && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
