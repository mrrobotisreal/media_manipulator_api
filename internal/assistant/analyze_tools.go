package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/analyze"
)

// FileResolver turns a turn's file ref into a local path the analyzers can
// read (MM116 implements it over the user's job uploads). The cleanup func is
// always safe to call.
type FileResolver interface {
	ResolveFile(ctx context.Context, userID, ref string) (path string, cleanup func(), err error)
}

// serverTool runs one server tool of this request: the analyze tools need the
// request's own meters and file ref, the rest are the knowledge tools.
func (r *run) serverTool(c ToolCall) string {
	switch c.Name {
	case ToolAnalyzeCompare:
		return r.analyzeCompare()
	case ToolAnalyzeImage, ToolAnalyzeAudio, ToolAnalyzeVideo:
		return r.analyzeFile(c)
	}
	text, _ := executeServerTool(c, r.surface, r.e.Knowledge)
	return text
}

// analyzeCompare diffs the browser's before/after meters of this turn's
// editResult (no file transfer). A new message carries only one reading.
func (r *run) analyzeCompare() string {
	b := &r.t.Body
	if er := b.EditResult; er != nil && len(er.MetricsBefore) > 0 && len(er.MetricsAfter) > 0 {
		return analyze.FormatCompare(er.MetricsBefore, er.MetricsAfter)
	}
	if b.Context != nil && len(b.Context.Metrics) > 0 {
		return "Only one reading exists so far (no edit to compare yet): " + b.Context.Metrics.String() + "."
	}
	if er := b.EditResult; er != nil && len(er.MetricsAfter) > 0 {
		return "Only the reading after the edit was sent: " + er.MetricsAfter.String() + ". There is no before reading to compare."
	}
	return "No meters were sent with this request (the preview is off or this editor has no meters). Judge by the digest and say what you would check."
}

// analyzeFile measures the turn's uploaded file. The ref must be the turn's
// own: the model cannot reach any other upload.
func (r *run) analyzeFile(c ToolCall) string {
	var args struct {
		FileRef string `json:"fileRef"`
	}
	if json.Unmarshal([]byte(c.Arguments), &args) != nil || args.FileRef == "" {
		return "Error: " + c.Name + " needs a fileRef."
	}
	if r.e.Files == nil || r.t.FileRef == "" || args.FileRef != r.t.FileRef {
		return "Error: no such file in this conversation."
	}
	path, cleanup, err := r.e.Files.ResolveFile(r.ctx, r.t.UserID, args.FileRef)
	if err != nil {
		return "Error: the file could not be opened."
	}
	defer cleanup()
	switch c.Name {
	case ToolAnalyzeImage:
		f, err := os.Open(path)
		if err != nil {
			return "Error: the file could not be opened."
		}
		defer f.Close()
		m, err := analyze.AnalyzeImage(f)
		if err != nil {
			return "Error: that file is not a picture I can measure."
		}
		var hues []string
		for _, h := range m.DominantHues {
			hues = append(hues, fmt.Sprintf("%.0f° (%.0f %%)", h.Hue, h.Share*100))
		}
		out := fmt.Sprintf("Picture %dx%d. %s.", m.Width, m.Height, m.Set().String())
		if len(hues) > 0 {
			out += " Dominant hues: " + strings.Join(hues, ", ") + "."
		}
		return out
	case ToolAnalyzeAudio:
		m, err := analyze.AnalyzeAudio(r.ctx, path)
		if err != nil {
			return "Error: that file has no audio I can measure."
		}
		return m.Set().String() + "."
	default:
		m, err := analyze.AnalyzeVideo(r.ctx, path)
		if err != nil {
			return "Error: that file is not a video I can measure."
		}
		out := fmt.Sprintf("Video %dx%d, %.1f s, %.2f fps, %s. %s.", m.Width, m.Height, m.DurationSec, m.FPS, m.VideoCodec, m.Set().String())
		if len(m.SceneLuma) > 0 {
			out += fmt.Sprintf(" Scene luma over time: %v.", m.SceneLuma)
		}
		return out
	}
}
