package services

import (
	"path/filepath"
	"strings"
)

// MM81: vocal stems follow the input's format.
//
// A remove_vocals job on an 87-minute MP3 (68 MB) used to come back as an
// 880 MB WAV: PCM adds no fidelity a compressed source never had. The rule is
// "never upsize a lossy source into PCM by default":
//
//   - lossy input (mp3, aac/m4a, ogg, opus, ac3) -> the same format; MP3 and
//     AAC stems are written at 320 kbps (StemEncoderArgs) so the separation
//     does not lose audible quality on top of the source's;
//   - lossless input (wav, flac, aiff, alac) -> FLAC (lossless, ~55 % of WAV);
//   - video input, or anything we cannot place -> MP3 320k.
//
// It applies when the caller sent no format or "auto". An explicit format
// (including "wav", which older UIs send) is honored exactly as before.

// StemFormatAuto is the format value that asks the API to pick.
const StemFormatAuto = "auto"

// IsStemOperation reports whether an AI audio operation produces a Demucs stem.
func IsStemOperation(op string) bool {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case AIAudioOpIsolateVocals, AIAudioOpRemoveVocals:
		return true
	}
	return false
}

// IsAutoFormat reports whether a requested format means "same as input".
func IsAutoFormat(format string) bool {
	f := strings.ToLower(strings.TrimSpace(format))
	return f == "" || f == StemFormatAuto
}

// ResolveStemFormat picks the stem output format (no dot) for an input, from
// its probed audio codec when known, else its file extension, else its MIME.
// It only ever returns a format audioEncoderArgs encodes natively: mp3, m4a,
// aac, ogg, opus, ac3 or flac. Pure.
func ResolveStemFormat(inputName, inputMime, audioCodec string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(strings.TrimSpace(inputName)), "."))
	mime := strings.ToLower(strings.TrimSpace(inputMime))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	// Vocals from a video's audio track: MP3 320k, whatever the track's codec.
	if strings.HasPrefix(mime, "video/") || isVideoExt(ext) {
		return "mp3"
	}

	// The probed codec is the most honest signal (an .m4a can hold ALAC).
	if codec := strings.ToLower(strings.TrimSpace(audioCodec)); codec != "" {
		switch {
		case codec == "flac" || codec == "alac" || strings.HasPrefix(codec, "pcm_"):
			return "flac"
		case codec == "mp3":
			return "mp3"
		case codec == "aac":
			if ext == "aac" {
				return "aac"
			}
			return "m4a"
		case codec == "vorbis":
			return "ogg"
		case codec == "opus":
			return "opus"
		case codec == "ac3":
			return "ac3"
		default:
			return "mp3"
		}
	}

	switch ext {
	case "mp3":
		return "mp3"
	case "aac":
		return "aac"
	case "m4a":
		return "m4a"
	case "ogg", "oga":
		return "ogg"
	case "opus":
		return "opus"
	case "ac3":
		return "ac3"
	case "wav", "wave", "flac", "aif", "aiff", "aifc", "alac", "caf":
		return "flac"
	}
	switch mime {
	case "audio/mpeg", "audio/mp3", "audio/x-mpeg":
		return "mp3"
	case "audio/aac":
		return "aac"
	case "audio/mp4", "audio/x-m4a", "audio/x-mp4a":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	case "audio/opus":
		return "opus"
	case "audio/flac", "audio/x-flac", "audio/wav", "audio/wave", "audio/x-wav", "audio/vnd.wave",
		"audio/aiff", "audio/x-aiff":
		return "flac"
	}
	return "mp3"
}

// NeedsCodecProbe reports whether the extension alone cannot tell lossy from
// lossless (an .m4a / .mp4 audio file may be AAC or ALAC; a .webm / .mkv /
// .caf may hold anything), so the upload should ask ffprobe for the codec.
func NeedsCodecProbe(inputName string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(strings.TrimSpace(inputName)), ".")) {
	case "m4a", "mp4", "m4v", "mov", "mkv", "webm", "caf", "":
		return true
	}
	return false
}

func isVideoExt(ext string) bool {
	switch ext {
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg", "3gp", "ts":
		return true
	}
	return false
}

// StemEncoderArgs is audioEncoderArgs for a vocal stem: MP3 and AAC/M4A are
// written at 320 kbps instead of the generic 192k. Every other format (and
// every other AI audio op) keeps audioEncoderArgs exactly.
func StemEncoderArgs(outputPath string) []string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(outputPath), ".")) {
	case "mp3":
		return []string{"-c:a", "libmp3lame", "-b:a", "320k"}
	case "aac", "m4a":
		return []string{"-c:a", "aac", "-b:a", "320k"}
	}
	return audioEncoderArgs(outputPath)
}
