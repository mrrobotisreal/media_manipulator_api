package models

// Track audibility (Darkroom v3 part 19). The Go mirror of
// lib/studio/trackAudibility.ts in the UI repo (part 18's solo semantics); the
// two are held together by the shared truth table in
// internal/models/testdata/track_audibility.json, which both repos test.
//
//   - Solo exists on AUDIO tracks only; any number of tracks may be soloed.
//   - While any audio track is soloed, ONLY soloed audio tracks are heard —
//     including a video track's embedded audio, which goes silent.
//   - Solo wins over mute.
//   - With no solo, a track is audible unless muted.
//
// Hidden (video tracks) is a visual flag and never affects audio.

// AnySolo reports whether any audio track is soloed.
func AnySolo(tracks []StudioTrack) bool {
	for _, t := range tracks {
		if t.Kind == StudioTrackKindAudio && t.Solo {
			return true
		}
	}
	return false
}

// IsTrackAudible reports whether the track contributes audio to the mix.
func IsTrackAudible(t StudioTrack, soloActive bool) bool {
	if soloActive {
		return t.Kind == StudioTrackKindAudio && t.Solo
	}
	return !t.Muted
}
