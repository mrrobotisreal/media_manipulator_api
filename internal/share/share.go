// Package share is the pure core of MM32 result links: token minting/hashing and
// the "what kind of result is this, and may it be shared" decision. No HTTP, no
// database, so the whole matrix is unit-testable.
package share

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	tokenBytes = 32
	// TokenLen is the length of a base64url (unpadded) 32-byte token.
	TokenLen = 43
)

// NewToken returns a fresh random token and its storage hash. The plaintext is
// shown once; only the hash is persisted.
func NewToken() (token, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is hex(sha256(token)): the primary key of mm_share_links.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// WellFormed reports whether s could be a token we minted, so garbage never
// reaches the database.
func WellFormed(s string) bool {
	if len(s) != TokenLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Kind mirrors the UI's ResultKind (lib/results/describe.ts).
type Kind string

const (
	KindImage      Kind = "image"
	KindVideo      Kind = "video"
	KindGIF        Kind = "gif"
	KindAudio      Kind = "audio"
	KindTranscript Kind = "transcript"
	KindCaptions   Kind = "captions"
	KindPDF        Kind = "pdf"
	KindArchive    Kind = "archive"
	KindHLS        Kind = "hls"
	KindDASH       Kind = "dash"
	KindMetadata   Kind = "metadata"
	KindUnknown    Kind = "unknown"
)

// Input is what the server knows about a finished job's result.
type Input struct {
	// FileName is the result file's name (extension decides the kind first).
	FileName string
	// Mime is the sniffed MIME of the result file (fallback signal).
	Mime string
	// Mode is the job's workflow mode (job.Mode / options.mode / recorded mode).
	Mode string
	// Tool is the recorded tool name.
	Tool string
}

var (
	imageExt   = set("jpg", "jpeg", "png", "webp", "avif", "bmp", "tiff", "tif", "ico", "svg", "heic")
	videoExt   = set("mp4", "webm", "mov", "mkv", "avi", "m4v")
	audioExt   = set("mp3", "wav", "m4a", "aac", "flac", "ogg", "opus")
	archiveExt = set("zip", "tar.gz", "tgz")
)

func set(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

func ext(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasSuffix(n, ".tar.gz") {
		return "tar.gz"
	}
	i := strings.LastIndex(n, ".")
	if i <= 0 || i == len(n)-1 {
		return ""
	}
	return n[i+1:]
}

func isMetadataLabel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "inspect_metadata", "metadata", "metadata_inspect", "metadata_report":
		return true
	}
	return false
}

func isStudio(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "studio")
}

// KindOf is describe() ported to the server. Order, as in the UI: extension
// (txt/json only count as a transcript for a transcribe job, .tar.gz is an
// HLS/DASH bundle for a transcode job), then workflow mode, then the sniffed
// MIME, then unknown. A metadata-inspect or Darkroom (studio_*) job is never
// anything but metadata/unknown-and-blocked, whatever its file looks like.
func KindOf(in Input) Kind {
	if isMetadataLabel(in.Mode) || isMetadataLabel(in.Tool) {
		return KindMetadata
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	e := ext(in.FileName)
	switch {
	case e == "gif":
		return KindGIF
	case imageExt[e]:
		return KindImage
	case videoExt[e]:
		return KindVideo
	case audioExt[e]:
		return KindAudio
	case e == "srt" || e == "vtt":
		return KindCaptions
	case e == "pdf":
		return KindPDF
	case e == "txt" || e == "json":
		if mode == "transcribe" || strings.EqualFold(in.Tool, "transcribe") {
			return KindTranscript
		}
	case archiveExt[e]:
		if mode == "transcode" {
			if strings.Contains(strings.ToLower(in.Tool), "dash") {
				return KindDASH
			}
			return KindHLS
		}
		return KindArchive
	}
	if mode == "transcribe" {
		return KindTranscript
	}
	m := strings.ToLower(strings.TrimSpace(in.Mime))
	switch {
	case m == "image/gif":
		return KindGIF
	case strings.HasPrefix(m, "image/"):
		return KindImage
	case strings.HasPrefix(m, "video/"):
		return KindVideo
	case strings.HasPrefix(m, "audio/"):
		return KindAudio
	case m == "application/pdf":
		return KindPDF
	}
	return KindUnknown
}

// Shareable reports whether a job may be shared at all. Never shareable:
// metadata inspector reports (GPS/PII), Darkroom (studio_*) projects and their
// renders, and unknown kinds. Account exports are not jobs, so they cannot be
// reached by a job id in the first place.
func Shareable(in Input, k Kind) bool {
	if BlockedWorkflow(in) {
		return false
	}
	return k != KindMetadata && k != KindUnknown && k != ""
}

// BlockedWorkflow is the part of Shareable that needs no file: the job's mode or
// tool already says metadata report or Darkroom.
func BlockedWorkflow(in Input) bool {
	return isStudio(in.Mode) || isStudio(in.Tool) || isMetadataLabel(in.Mode) || isMetadataLabel(in.Tool)
}
