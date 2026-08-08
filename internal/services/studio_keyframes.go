package services

import (
	"fmt"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Keyframe curve math (part 15) — the Go mirror of lib/studio/ease.ts, THE
// cross-side contract for how keyframed values interpolate. Both sides are
// fixture-tested against the same value table (testdata/
// studio_keyframe_curves.json, a byte-identical copy of the UI repo's
// lib/studio/ease.fixtures.json). Change the curves in both places or neither.
//
// Curve definitions, on normalized segment progress f ∈ [0,1]:
//
//	linear    f
//	hold      0 (the outgoing value holds until the next keyframe)
//	easeIn    f²
//	easeOut   1 − (1−f)²
//	easeBoth  f < 0.5 ? 2f² : 1 − 2(1−f)²   (the research ticket 01 curve)
//
// SEGMENT EASE COMES FROM THE LEFT KEYFRAME: [a, b) interpolates a.value →
// b.value using a.ease; at exactly b.t the value IS b's (hold jumps AT the
// next keyframe, matching the [START,END) sendcmd intervals). Before the first
// keyframe the first value holds; after the last, the last holds.

// KeyframeEasedProgress maps linear progress f (clamped 0..1) through an ease
// curve. Mirrors easedProgress in lib/studio/ease.ts.
func KeyframeEasedProgress(ease string, f float64) float64 {
	p := f
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	switch ease {
	case "hold":
		return 0
	case "easeIn":
		return p * p
	case "easeOut":
		return 1 - (1-p)*(1-p)
	case "easeBoth":
		if p < 0.5 {
			return 2 * p * p
		}
		return 1 - 2*(1-p)*(1-p)
	default: // linear + anything the sanitizer let through
		return p
	}
}

// KeyframeValueAt evaluates a keyframe lane at clip-local time t. Mirrors
// valueAt in lib/studio/ease.ts. Lanes are assumed sorted (SanitizeTracks
// normalizes). Returns fallback for an empty lane.
func KeyframeValueAt(kfs []models.StudioKeyframe, t, fallback float64) float64 {
	if len(kfs) == 0 {
		return fallback
	}
	if t <= kfs[0].T {
		return kfs[0].Value
	}
	last := kfs[len(kfs)-1]
	if t >= last.T {
		return last.Value
	}
	for i := 1; i < len(kfs); i++ {
		b := kfs[i]
		if t < b.T {
			a := kfs[i-1]
			span := b.T - a.T
			if span <= 0 {
				return b.Value
			}
			f := (t - a.T) / span
			return a.Value + (b.Value-a.Value)*KeyframeEasedProgress(a.Ease, f)
		}
	}
	return last.Value
}

// --- ffmpeg expression emission ---------------------------------------------

// easedProgressExpr states an ease curve as an ffmpeg expression of the linear
// progress expression fExpr (already clamped 0..1 by construction inside a
// piecewise branch). The cubics are the exact curves above.
func easedProgressExpr(ease, fExpr string) string {
	switch ease {
	case "easeIn":
		return fmt.Sprintf("pow(%s,2)", fExpr)
	case "easeOut":
		return fmt.Sprintf("(1-pow(1-%s,2))", fExpr)
	case "easeBoth":
		return fmt.Sprintf("if(lt(%s,0.5),2*pow(%s,2),1-2*pow(1-%s,2))", fExpr, fExpr, fExpr)
	default: // linear ("hold" is handled by the segment emitter — constant a)
		return fExpr
	}
}

// keyframeSegmentExpr states one segment's value: a + (b−a)·ease(f) with
// f = (T−a.t)/span, T being the caller's time expression (clip-local "t" for
// audio chains, "(t-<timelineStart>)" for video chains).
func keyframeSegmentExpr(a, b models.StudioKeyframe, timeExpr string) string {
	if a.Ease == "hold" {
		return formatGain(a.Value)
	}
	span := b.T - a.T
	if span <= 0 {
		return formatGain(b.Value)
	}
	f := fmt.Sprintf("(%s-%s)/%s", timeExpr, formatSeconds(a.T), formatSeconds(span))
	return fmt.Sprintf("(%s+(%s)*%s)", formatGain(a.Value), formatGain(b.Value-a.Value), easedProgressExpr(a.Ease, f))
}

// KeyframeExpr builds the full piecewise expression for a lane, honoring ease:
// nested if(lt(T,…),…) branches from first to last keyframe, holding the first
// value before and the last value after — the expression twin of
// KeyframeValueAt (and the eased generalization of the legacy volumeExpr).
func KeyframeExpr(kfs []models.StudioKeyframe, timeExpr string) string {
	n := len(kfs)
	if n == 0 {
		return formatGain(0)
	}
	if n == 1 {
		return formatGain(kfs[0].Value)
	}
	expr := formatGain(kfs[n-1].Value) // hold after the last point
	for i := n - 2; i >= 0; i-- {
		seg := keyframeSegmentExpr(kfs[i], kfs[i+1], timeExpr)
		expr = fmt.Sprintf("if(lt(%s,%s),%s,%s)", timeExpr, formatSeconds(kfs[i+1].T), seg, expr)
	}
	return fmt.Sprintf("if(lt(%s,%s),%s,%s)", timeExpr, formatSeconds(kfs[0].T), formatGain(kfs[0].Value), expr)
}

// --- lane addressing ---------------------------------------------------------

// effectLane returns the "<effectId>.<param>" lane on a clip's keyframes, nil
// when absent.
func effectLane(k *models.StudioClipKeyframes, effectID, param string) []models.StudioKeyframe {
	if k == nil || k.Effects == nil || effectID == "" {
		return nil
	}
	return k.Effects[effectID+"."+param]
}

// hasLane is a nil-safe non-empty check.
func hasLane(kfs []models.StudioKeyframe) bool { return len(kfs) > 0 }

// maxLaneValue returns the maximum keyframed value (used for the animated-scale
// bounding box), or def for an empty lane.
func maxLaneValue(kfs []models.StudioKeyframe, def float64) float64 {
	if len(kfs) == 0 {
		return def
	}
	m := kfs[0].Value
	for _, k := range kfs[1:] {
		if k.Value > m {
			m = k.Value
		}
	}
	return m
}

// videoClipTimeExpr is the clip-local time expression inside a video clip
// chain, whose frames carry TIMELINE pts (setpts=PTS-STARTPTS+start/TB).
func videoClipTimeExpr(timelineStart float64) string {
	if timelineStart == 0 {
		return "t"
	}
	return fmt.Sprintf("(t-%s)", formatSeconds(timelineStart))
}

// keyframesOf is a nil-safe accessor for a seg's keyframe container.
func keyframesOf(k *models.StudioClipKeyframes) models.StudioClipKeyframes {
	if k == nil {
		return models.StudioClipKeyframes{}
	}
	return *k
}

// quoteChainExpr single-quotes a per-frame expression used as a filter option
// inside a comma-joined chain (crop/scale/rotate style), since piecewise
// expressions always contain commas.
func quoteChainExpr(e string) string {
	if strings.ContainsAny(e, ",:") {
		return "'" + e + "'"
	}
	return e
}
