package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Shared frame + vision-language-model helpers (MM74, design
// docs/design/VIDEO_SUMMARY.md §3 "Reuse, don't fork").
//
// Two callers use them with their own prompts and outputs:
//   - AnalysisQueue (analysis_queue.go): internal product intelligence, fixed
//     interval frames, raw Ollama responses persisted as-is.
//   - VideoSummaryService (video_summary.go): the user-facing summary, scene-aware
//     frames with real timestamps, schema-validated JSON with one repair retry.
//
// Nothing here knows about either product; it is ffmpeg + Ollama plumbing.

// Frame is one extracted still and the source timestamp it was taken at.
type Frame struct {
	Path    string  `json:"path"`
	TimeSec float64 `json:"timeSec"`
}

// FramePaths returns the file paths of frames, in order.
func FramePaths(frames []Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Path)
	}
	return out
}

// extractIntervalFrames samples one frame every interval seconds (at most
// maxFrames, scaled to maxWidth) into framesDir/frame_%05d.jpg. This is the
// analysis queue's sampler, moved here unchanged: the ffmpeg arguments are
// byte-identical to the pre-MM74 AnalysisQueue.extractFrames. TimeSec is the
// nominal interval position (index × interval); the analysis queue only ever
// used the paths.
func extractIntervalFrames(ctx context.Context, inputPath, framesDir string, interval, maxWidth, maxFrames int) ([]Frame, error) {
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return nil, err
	}
	pattern := filepath.Join(framesDir, "frame_%05d.jpg")
	filter := fmt.Sprintf("fps=1/%d,scale=%d:-2:flags=lanczos", interval, maxWidth)
	_, stderr, err := runCommand(ctx, "ffmpeg", "-y", "-i", inputPath, "-vf", filter, "-q:v", "3", "-frames:v", strconv.Itoa(maxFrames), pattern)
	if err != nil {
		return nil, fmt.Errorf("extract frames: %w: %s", err, tail(stderr, 1200))
	}
	paths, err := filepath.Glob(filepath.Join(framesDir, "frame_*.jpg"))
	if err != nil {
		return nil, err
	}
	frames := make([]Frame, 0, len(paths))
	for i, p := range paths {
		frames = append(frames, Frame{Path: p, TimeSec: float64(i * interval)})
	}
	return frames, nil
}

// showinfoPTSTime matches the pts_time field of ffmpeg's showinfo filter log
// lines ("[Parsed_showinfo_1 @ 0x...] n:   0 pts:  12345 pts_time:12.345 ...").
var showinfoPTSTime = regexp.MustCompile(`Parsed_showinfo[^\]]*\].*?\bpts_time:\s*(-?[0-9]+(?:\.[0-9]+)?)`)

// parseShowinfoTimes extracts every frame timestamp showinfo logged, in log
// order. Lines from other filters, and malformed values, are ignored.
func parseShowinfoTimes(stderr string) []float64 {
	var out []float64
	for _, line := range strings.Split(stderr, "\n") {
		m := showinfoPTSTime.FindStringSubmatch(line)
		if len(m) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil || v < 0 {
			continue
		}
		out = append(out, v)
	}
	return out
}

// detectSceneTimes runs one decode pass with ffmpeg's scene score and returns
// the timestamps of the frames whose score exceeds threshold (the cuts), read
// from showinfo. No image is written: which frames to keep is PlanFrames'
// decision, made in Go so it can be tested.
func detectSceneTimes(ctx context.Context, inputPath string, threshold float64) ([]float64, error) {
	filter := fmt.Sprintf("select='gt(scene,%s)',showinfo", strconv.FormatFloat(threshold, 'f', 2, 64))
	_, stderr, err := runCommand(ctx, "ffmpeg", "-hide_banner", "-nostats", "-i", inputPath, "-an", "-vf", filter, "-vsync", "vfr", "-f", "null", "-")
	if err != nil {
		return nil, fmt.Errorf("scene detection: %w: %s", err, tail(stderr, 1200))
	}
	times := parseShowinfoTimes(stderr)
	sort.Float64s(times)
	return times, nil
}

// frameFileName names a frame by its timestamp, zero-padded so a directory
// listing sorts chronologically: frame_0012.480.jpg.
func frameFileName(t float64) string {
	return fmt.Sprintf("frame_%08.3f.jpg", t)
}

// extractFramesAt writes one JPEG per requested timestamp (input-seeked, so it
// is cheap even on long sources), scaled to maxWidth. A timestamp ffmpeg could
// not produce a picture for (the very end of a stream) is skipped rather than
// failing the whole set; an error is returned only when nothing was produced.
func extractFramesAt(ctx context.Context, inputPath, framesDir string, times []float64, maxWidth int) ([]Frame, error) {
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return nil, err
	}
	scale := fmt.Sprintf("scale=%d:-2:flags=lanczos", maxWidth)
	frames := make([]Frame, 0, len(times))
	var lastErr error
	for _, t := range times {
		out := filepath.Join(framesDir, frameFileName(t))
		_, stderr, err := runCommand(ctx, "ffmpeg", "-y", "-hide_banner", "-nostats",
			"-ss", strconv.FormatFloat(t, 'f', 3, 64), "-i", inputPath,
			"-frames:v", "1", "-an", "-vf", scale, "-q:v", "3", out)
		if ctx.Err() != nil {
			return frames, ctx.Err()
		}
		if err != nil {
			lastErr = fmt.Errorf("extract frame at %.3fs: %w: %s", t, err, tail(stderr, 400))
			continue
		}
		if info, statErr := os.Stat(out); statErr != nil || info.Size() == 0 {
			continue
		}
		frames = append(frames, Frame{Path: out, TimeSec: t})
	}
	if len(frames) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return frames, nil
}

// encodeImagesBase64 reads image files for Ollama's `images` field.
func encodeImagesBase64(paths []string) ([]string, error) {
	images := make([]string, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		images = append(images, base64.StdEncoding.EncodeToString(body))
	}
	return images, nil
}

// ollamaChat POSTs one /api/chat payload and returns the decoded response
// object. A non-2xx status is an error carrying the body.
func ollamaChat(ctx context.Context, client *http.Client, baseURL string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(baseURL, "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama status %d: %v", resp.StatusCode, response)
	}
	return response, nil
}

// ollamaMessageContent is the assistant text of a /api/chat response.
func ollamaMessageContent(response map[string]any) string {
	if message, ok := response["message"].(map[string]any); ok {
		if content, ok := message["content"].(string); ok {
			return strings.TrimSpace(content)
		}
	}
	return ""
}

// analysisOllamaClient is the client + base URL the analysis queue has always
// used: OLLAMA_URL and OLLAMA_TIMEOUT_SECONDS read per call.
func analysisOllamaClient() (*http.Client, string) {
	client := &http.Client{Timeout: time.Duration(envInt("OLLAMA_TIMEOUT_SECONDS", 300)) * time.Second}
	return client, envOrDefault("OLLAMA_URL", "http://localhost:11434")
}

// JSONChatRequest is one Ollama /api/chat call that must answer with a JSON
// object (`format: json`).
type JSONChatRequest struct {
	Client  *http.Client
	BaseURL string
	Model   string
	System  string
	User    string
	// Images are base64 JPEG/PNG payloads attached to the user message (VLM).
	Images []string
	// Options is Ollama's `options` object (num_ctx, temperature, ...).
	Options map[string]any
	// KeepAlive overrides the daemon's OLLAMA_KEEP_ALIVE for this call ("5m",
	// "0"). Empty leaves the daemon default.
	KeepAlive string
}

// JSONChatResult is what ChatJSON returns: the validated JSON and how many
// model calls it took (1, or 2 when the repair retry ran).
type JSONChatResult struct {
	Content  []byte
	Attempts int
}

// repairPrompt is sent once, after a reply that did not parse or validate.
const repairPrompt = "Your previous reply was not valid for the required JSON schema: %s. Reply again with ONLY the corrected JSON object. No prose, no code fences."

// ChatJSON calls the model with `format: json` and validates the reply. On an
// invalid reply it makes exactly one repair call (the original conversation,
// the bad reply as the assistant turn, and the validation error); a second
// invalid reply is an error. validate receives the raw JSON text and returns a
// user-facing reason when it is unacceptable; nil validate accepts any JSON
// object.
func ChatJSON(ctx context.Context, req JSONChatRequest, validate func([]byte) error) (JSONChatResult, error) {
	if req.Client == nil {
		req.Client = http.DefaultClient
	}
	messages := make([]map[string]any, 0, 4)
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	user := map[string]any{"role": "user", "content": req.User}
	if len(req.Images) > 0 {
		user["images"] = req.Images
	}
	messages = append(messages, user)

	check := func(content string) ([]byte, error) {
		raw := []byte(stripCodeFence(content))
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("not a JSON object (%v)", err)
		}
		if validate != nil {
			if err := validate(raw); err != nil {
				return nil, err
			}
		}
		return raw, nil
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		payload := map[string]any{
			"model":    req.Model,
			"stream":   false,
			"messages": messages,
			"format":   "json",
		}
		if len(req.Options) > 0 {
			payload["options"] = req.Options
		}
		if req.KeepAlive != "" {
			payload["keep_alive"] = req.KeepAlive
		}
		resp, err := ollamaChat(ctx, req.Client, req.BaseURL, payload)
		if err != nil {
			// Transport/model failures are not "invalid JSON": no repair call.
			return JSONChatResult{Attempts: attempt}, err
		}
		content := ollamaMessageContent(resp)
		raw, verr := check(content)
		if verr == nil {
			return JSONChatResult{Content: raw, Attempts: attempt}, nil
		}
		lastErr = verr
		messages = append(messages,
			map[string]any{"role": "assistant", "content": content},
			map[string]any{"role": "user", "content": fmt.Sprintf(repairPrompt, verr.Error())},
		)
	}
	return JSONChatResult{Attempts: 2}, fmt.Errorf("model reply invalid after one repair retry: %w", lastErr)
}

// stripCodeFence removes a ```json ... ``` wrapper some models add even with
// format: json.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
