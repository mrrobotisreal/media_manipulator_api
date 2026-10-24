package models

// Speed controls (Darkroom v3 part 17). The EDL stores a clip's SOURCE span
// (SourceIn..SourceOut) plus an optional constant Speed; the clip's TIMELINE
// duration is derived: (SourceOut - SourceIn) / speed. Freeze clips are held
// frames whose duration is simply SourceOut - SourceIn. These helpers are the
// single Go definition of that rule; lib/studio/speed.ts mirrors them.

// ClipSpeed is the clip's effective constant rate: 1 when absent, for freeze
// clips (a held frame has no rate), and for title clips; otherwise Speed
// clamped to the registry range.
func ClipSpeed(c StudioClip) float64 {
	if c.Freeze != nil || c.Title != nil || c.Speed == nil {
		return 1
	}
	return clampF(*c.Speed, studioSpeedMin, studioSpeedMax)
}

// ClipTimelineDuration is how long the clip occupies on the timeline.
func ClipTimelineDuration(c StudioClip) float64 {
	d := c.SourceOut - c.SourceIn
	if d <= 0 {
		return 0
	}
	return d / ClipSpeed(c)
}

// ClipMaintainsPitch reports the "Maintain audio pitch" toggle (default on).
func ClipMaintainsPitch(c StudioClip) bool {
	return c.MaintainPitch == nil || *c.MaintainPitch
}
