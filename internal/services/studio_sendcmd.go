package services

import (
	"fmt"
	"math"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Sendcmd sidecar generation (part 15, per research ticket 01's verdict).
//
// Effect parameters (Lumetri color, chromakey — backend-capable) plus the two
// non-expression-capable per-clip params (opacity via colorchannelmixer aa,
// pan via stereotools balance_in) are animated with ffmpeg's `sendcmd` filter:
// ONE timeline-clocked instance (on [vbase]; asendcmd on the mix when the
// project has no video) drives every commandable filter in the graph by
// instance name.
//
// The generated file is a pure string artifact — golden-tested exactly like
// the filter graphs. Per the research answer's gotchas:
//   - intervals are [START,END) and TI never reaches 1.0 → every lane ends
//     with an explicit [enter] pin at its last keyframe's time;
//   - targets are always instance ids (eq@lumq0), never bare filter names —
//     the ids derive from the seg's SORTED index, matching the chain emission
//     in buildVideoClipChain / audioSegChain;
//   - values are clamped upstream (SanitizeTracks) — ffmpeg silently ignores
//     invalid command args, so the generator never relies on it erroring;
//   - the carrier stream is CFR at the project rate (the color base), so
//     intervals fire deterministically. Requires ffmpeg ≥ 4.4 (sendcmd
//     [expr] + commandable filters); see assertStudioSendCmdSupport.
//
// The [expr] argument re-evaluates per frame with TI = 0→1 progress through
// the interval, so one line per keyframe segment yields a frame-accurate ramp:
//
//	2.000-4.500 [expr] eq@lumq0 contrast 'lerp(1.000,1.500,TI)';
//	4.500 [enter] eq@lumq0 contrast 1.500;
//
// Ease curves ride the same shared cubics as everything else: the third lerp
// argument is easedProgressExpr(ease, "TI"). 'hold' segments become [enter]
// pins at the segment start (constant until the next keyframe).

// sendCmdTarget is one (filter instance, option) a lane drives. Endpoint
// values are transformed by `scale` (linear maps like temp→rm = v/100) or
// wrapped by pow2 (exposure→gain, the one non-linear map).
type sendCmdTarget struct {
	filter string  // e.g. "eq@lumq0"
	option string  // e.g. "contrast"
	scale  float64 // endpoint multiplier (1 when unused)
	pow2   bool    // value = 2^v (exposure)
}

func (tg sendCmdTarget) exprArg(a, b models.StudioKeyframe) string {
	e := easedProgressExpr(a.Ease, "TI")
	if tg.pow2 {
		return fmt.Sprintf("pow(2,lerp(%s,%s,%s))", formatGain(a.Value), formatGain(b.Value), e)
	}
	return fmt.Sprintf("lerp(%s,%s,%s)", formatGain(a.Value*tg.scale), formatGain(b.Value*tg.scale), e)
}

func (tg sendCmdTarget) enterArg(v float64) string {
	if tg.pow2 {
		return formatGain(math.Pow(2, v))
	}
	return formatGain(v * tg.scale)
}

// appendLaneCommands writes one lane's interval lines. All targets of the lane
// share each interval line (comma-joined commands), keeping the file compact
// and the firing order deterministic.
func appendLaneCommands(sb *strings.Builder, lane []models.StudioKeyframe, timelineStart float64, targets []sendCmdTarget) {
	if len(lane) < 2 || len(targets) == 0 {
		return // a single keyframe is fully covered by the chain's static seed
	}
	for i := 0; i < len(lane)-1; i++ {
		a, b := lane[i], lane[i+1]
		s := formatSeconds(timelineStart + a.T)
		e := formatSeconds(timelineStart + b.T)
		cmds := make([]string, 0, len(targets))
		if a.Ease == "hold" {
			for _, tg := range targets {
				cmds = append(cmds, fmt.Sprintf("[enter] %s %s %s", tg.filter, tg.option, tg.enterArg(a.Value)))
			}
			sb.WriteString(s + " " + strings.Join(cmds, ", ") + ";\n")
			continue
		}
		for _, tg := range targets {
			cmds = append(cmds, fmt.Sprintf("[expr] %s %s '%s'", tg.filter, tg.option, tg.exprArg(a, b)))
		}
		sb.WriteString(s + "-" + e + " " + strings.Join(cmds, ", ") + ";\n")
	}
	// Terminal pin: TI never reaches 1.0 inside [START,END).
	last := lane[len(lane)-1]
	cmds := make([]string, 0, len(targets))
	for _, tg := range targets {
		cmds = append(cmds, fmt.Sprintf("[enter] %s %s %s", tg.filter, tg.option, tg.enterArg(last.Value)))
	}
	sb.WriteString(formatSeconds(timelineStart+last.T) + " " + strings.Join(cmds, ", ") + ";\n")
}

// one is the identity target constructor.
func target(filter, option string) sendCmdTarget {
	return sendCmdTarget{filter: filter, option: option, scale: 1}
}

// GenerateStudioSendCmd renders the keyframe sidecar for a plan. Video segs
// are walked in sortVideoSegs order (the SAME order buildMultiTrackExportArgs
// compiles them, so index k matches the chain's instance names); audio segs by
// their SegIndex. Returns "" when nothing in the plan needs the sidecar.
func GenerateStudioSendCmd(plan StudioExportPlan) string {
	var sb strings.Builder
	for k, vc := range sortVideoSegs(plan.Video) {
		kf := keyframesOf(vc.ClipKeyframes)
		appendLaneCommands(&sb, kf.Opacity, vc.TimelineStart, []sendCmdTarget{
			target(fmt.Sprintf("colorchannelmixer@op%d", k), "aa"),
		})
		lumetri, _, chroma := pickEffects(vc.Effects)
		if lumetri != nil {
			id := lumetri.ID
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "exposure"), vc.TimelineStart, []sendCmdTarget{
				{filter: fmt.Sprintf("colorchannelmixer@lume%d", k), option: "rr", pow2: true},
				{filter: fmt.Sprintf("colorchannelmixer@lume%d", k), option: "gg", pow2: true},
				{filter: fmt.Sprintf("colorchannelmixer@lume%d", k), option: "bb", pow2: true},
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "temperature"), vc.TimelineStart, []sendCmdTarget{
				{filter: fmt.Sprintf("colorbalance@lumb%d", k), option: "rm", scale: 1.0 / 100},
				{filter: fmt.Sprintf("colorbalance@lumb%d", k), option: "bm", scale: -1.0 / 100},
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "tint"), vc.TimelineStart, []sendCmdTarget{
				{filter: fmt.Sprintf("colorbalance@lumb%d", k), option: "gm", scale: -1.0 / 100},
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "vibrance"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("vibrance@lumv%d", k), "intensity"),
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "contrast"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("eq@lumq%d", k), "contrast"),
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "saturation"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("eq@lumq%d", k), "saturation"),
			})
		}
		if chroma != nil {
			id := chroma.ID
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "similarity"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("chromakey@ck%d", k), "similarity"),
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "blend"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("chromakey@ck%d", k), "blend"),
			})
			appendLaneCommands(&sb, effectLane(vc.ClipKeyframes, id, "despill"), vc.TimelineStart, []sendCmdTarget{
				target(fmt.Sprintf("despill@dsp%d", k), "mix"),
			})
		}
	}
	for _, ac := range plan.Audio {
		kf := keyframesOf(ac.ClipKeyframes)
		appendLaneCommands(&sb, kf.Pan, ac.TimelineStart, []sendCmdTarget{
			target(fmt.Sprintf("stereotools@pan%d", ac.SegIndex), "balance_in"),
		})
	}
	return sb.String()
}
