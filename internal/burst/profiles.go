// Package burst is the GPU burst core (MM124, docs/design/GPU_BURST_EC2.md,
// ADR-0020): config-driven workload profiles, the burst_runs ledger with hard
// spend caps, an on-demand EC2 provider, the Tailscale key minter, the reaper
// and the planner's launch/terminate bookkeeping. It never imports
// internal/services; services import burst.
package burst

import "strings"

// Worker families (design §9): which image holds a profile's runtime.
const (
	FamilyImage   = "image"
	FamilyAudio   = "audio"
	FamilyVideo   = "video"
	FamilyRestore = "restore"
	FamilyOllama  = "ollama"
	FamilyVoices  = "voices"
)

// Profile is one workload's hardware needs (design §5).
type Profile struct {
	// Key is the table key and the BURST_WORKLOADS value.
	Key string
	// Prefix marks a row matched by Kind prefix ("restore-"), Key otherwise
	// matched exactly or as Key+"[...]" for ollama.
	Prefix bool
	Wave   int
	Family string
	// MinGPUMemGiB is the minimum GPU memory.
	MinGPUMemGiB int
	// Candidates are ordered instance types, overridable by
	// BURST_INSTANCE_TYPES_<FAMILY>.
	Candidates      []string
	RootGiB         int
	ScratchGiB      int
	Prewarm         []string
	SeedHoldSeconds int
	RemoteTimeout   int // seconds
	// Never rows are in the table explicitly so a new GPU tool cannot silently
	// become burstable or be forgotten.
	Never       bool
	NeverReason string
}

var (
	candSmall   = []string{"g6.xlarge", "g7.2xlarge", "g6e.xlarge"}
	candAudio   = []string{"g6.xlarge", "g7.2xlarge"}
	candRestore = []string{"g6.xlarge", "g7.2xlarge", "g6e.xlarge"}
)

func img(key string, extra ...string) Profile {
	return Profile{Key: key, Wave: 1, Family: FamilyImage, MinGPUMemGiB: 8, Candidates: candSmall,
		RootGiB: 100, ScratchGiB: 20, Prewarm: extra, SeedHoldSeconds: 30, RemoteTimeout: 900}
}

func never(key, reason string) Profile {
	return Profile{Key: key, Never: true, NeverReason: reason}
}

// profiles is the single table (design §5).
var profiles = []Profile{
	img("remove_background"),
	img("ai_upscale"),
	img("redact_text"),
	img("remove_object"),
	{Key: "deepfilter", Wave: 1, Family: FamilyAudio, MinGPUMemGiB: 8, Candidates: candAudio, RootGiB: 80, ScratchGiB: 20, SeedHoldSeconds: 20, RemoteTimeout: 900},
	{Key: "demucs", Wave: 1, Family: FamilyAudio, MinGPUMemGiB: 8, Candidates: candAudio, RootGiB: 80, ScratchGiB: 20, SeedHoldSeconds: 60, RemoteTimeout: 1800},
	{Key: "whisper-ct2", Wave: 1, Family: FamilyAudio, MinGPUMemGiB: 8, Candidates: candAudio, RootGiB: 80, ScratchGiB: 20, SeedHoldSeconds: 60, RemoteTimeout: 1800},
	{Key: "frame_interpolation", Wave: 1, Family: FamilyVideo, MinGPUMemGiB: 8, Candidates: candAudio, RootGiB: 100, ScratchGiB: 60, SeedHoldSeconds: 120, RemoteTimeout: 3600},
	{Key: "restore-", Prefix: true, Wave: 1, Family: FamilyRestore, MinGPUMemGiB: 16, Candidates: candRestore, RootGiB: 150, ScratchGiB: 100, SeedHoldSeconds: 600, RemoteTimeout: 7200},
	{Key: "image-restore-", Prefix: true, Wave: 1, Family: FamilyRestore, MinGPUMemGiB: 12, Candidates: candRestore, RootGiB: 150, ScratchGiB: 20, SeedHoldSeconds: 60, RemoteTimeout: 1800},
	{Key: "ollama", Wave: 2, Family: FamilyOllama, MinGPUMemGiB: 24, Candidates: candAudio, RootGiB: 150, ScratchGiB: 20, SeedHoldSeconds: 90, RemoteTimeout: 1800},
	{Key: "voices-", Prefix: true, Wave: 3, Family: FamilyVoices, MinGPUMemGiB: 16, Candidates: candAudio, RootGiB: 120, ScratchGiB: 20, SeedHoldSeconds: 60, RemoteTimeout: 1800},

	never("face_privacy", "the script is CPU OpenCV; the GPU slot is nominal (recommend dropping it)"),
	never("face_privacy[detect]", "interactive face detection; editor latency"),
	never("rembg[photo]", "Aperture interactive subject mask; editor latency"),
	never("lama[photo]", "Aperture interactive heal; editor latency"),
	never("whisper-ct2[analysis]", "background analysis queue; never burst"),
	never("whisper-ct2[summary]", "video-summary step runs inside the summary pipeline; not in wave 1"),
	never("document-scan-preclean", "document-scan secondary stage on a local card; not in wave 1"),
	never("document-scan-", "document-scan secondary stages run on a local card; only their ollama[...] primaries burst (wave 2)"),
}

// Profiles returns a copy of the table.
func Profiles() []Profile {
	return append([]Profile(nil), profiles...)
}

// Lookup resolves a scheduler Kind to its row. ok is false for a Kind the table
// does not know (never burstable). Exact keys win over prefixes, so
// "whisper-ct2[analysis]" is never matched by "whisper-ct2".
func Lookup(kind string) (Profile, bool) {
	for _, p := range profiles {
		if !p.Prefix && p.Key == kind {
			return p, true
		}
	}
	for _, p := range profiles {
		if p.Prefix && strings.HasPrefix(kind, p.Key) {
			return p, true
		}
	}
	// ollama[caller] family.
	if strings.HasPrefix(kind, "ollama[") && strings.HasSuffix(kind, "]") {
		for _, p := range profiles {
			if p.Key == "ollama" {
				return p, true
			}
		}
	}
	return Profile{}, false
}

// Burstable reports whether a Kind may ever run on a burst instance.
func Burstable(kind string) bool {
	p, ok := Lookup(kind)
	return ok && !p.Never
}

// CandidatesFor returns the instance types to try: the env override for the
// family wins over the profile's own list.
func CandidatesFor(p Profile, overrides map[string][]string) []string {
	if o := overrides[strings.ToUpper(p.Family)]; len(o) > 0 {
		return o
	}
	return p.Candidates
}

// GPUMemGiB is the GPU memory of an instance type's single GPU (design §5), 0
// when unknown. The planner uses it to decide whether a booting instance can be
// adopted by another waiter.
func GPUMemGiB(instanceType string) int {
	switch {
	case strings.HasPrefix(instanceType, "g6e."):
		return 48
	case strings.HasPrefix(instanceType, "g6."):
		return 24
	case strings.HasPrefix(instanceType, "g7."):
		return 32
	}
	return 0
}
