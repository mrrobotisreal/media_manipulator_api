package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// ---------------------------------------------------------------------------
// Fakes: Ollama routed by model, a lease recorder, ffmpeg/Whisper steps.
// ---------------------------------------------------------------------------

type summaryOllama struct {
	mu        sync.Mutex
	vlmCalls  []map[string]any
	textCalls []map[string]any
	vlmReply  func(call int) string
	textReply func(call int) string
	srv       *httptest.Server
	hits      map[string]int // base URL tag -> calls
}

func newSummaryOllama(t *testing.T, vlmModel string) *summaryOllama {
	t.Helper()
	f := &summaryOllama{hits: map[string]int{}}
	f.vlmReply = func(int) string {
		return `{"scene":"A presenter at a desk","onScreenText":["WELCOME"],"people":["one presenter"],"objects":["laptop"],"actions":["talking"],"cameraNotes":"static"}`
	}
	f.textReply = func(int) string { return fixtureSummaryReply }
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.mu.Lock()
		f.hits[r.Host]++
		var reply string
		if p["model"] == vlmModel {
			f.vlmCalls = append(f.vlmCalls, p)
			reply = f.vlmReply(len(f.vlmCalls))
		} else {
			f.textCalls = append(f.textCalls, p)
			reply = f.textReply(len(f.textCalls))
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": reply}, "done": true})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type leaseEvent struct {
	task     gpu.TaskType
	acquired bool
	at       time.Time
}

type fakeLeaser struct {
	mu     sync.Mutex
	events []leaseEvent
}

type fakeLease struct {
	l    *fakeLeaser
	task gpu.TaskType
}

func (f *fakeLeaser) Acquire(_ context.Context, task gpu.TaskType, tool, _, _ string) (summaryLease, error) {
	if tool != models.VideoSummaryTool {
		return nil, fmt.Errorf("lease tool = %q", tool)
	}
	f.mu.Lock()
	f.events = append(f.events, leaseEvent{task: task, acquired: true, at: time.Now()})
	f.mu.Unlock()
	return &fakeLease{l: f, task: task}, nil
}

func (l *fakeLease) Release(context.Context, error) {
	l.l.mu.Lock()
	l.l.events = append(l.l.events, leaseEvent{task: l.task, acquired: false, at: time.Now()})
	l.l.mu.Unlock()
}

func (f *fakeLeaser) balanced(task gpu.TaskType) (acq, rel int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.task != task {
			continue
		}
		if e.acquired {
			acq++
		} else {
			rel++
		}
	}
	return
}

// fakeFrames writes a small file per requested time.
func fakeFrames(_ context.Context, _ string, dir string, times []float64, _ int) ([]Frame, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := make([]Frame, 0, len(times))
	for _, t := range times {
		p := filepath.Join(dir, frameFileName(t))
		if err := os.WriteFile(p, []byte("jpeg"), 0o644); err != nil {
			return nil, err
		}
		out = append(out, Frame{Path: p, TimeSec: t})
	}
	return out, nil
}

func speechTranscript() models.VideoSummaryTranscript {
	return models.VideoSummaryTranscript{Language: "en", Text: "Hello and welcome. Today we build a birdhouse.", Segments: []models.VideoSummaryTranscriptSegment{
		{Start: 1, End: 4, Text: "Hello and welcome."},
		{Start: 50, End: 55, Text: "Today we build a birdhouse."},
		{Start: 150, End: 160, Text: "And it is finished."},
	}}
}

const fixtureSummaryReply = `{
  "title": "Building a birdhouse from one pine board",
  "summary": "A presenter introduces a small woodworking project. She builds a birdhouse from a single pine board. The panels are cut, glued and nailed together. The finished birdhouse is shown at the end.",
  "chapters": [
    {"start": 0, "end": 45, "title": "Introduction", "description": "The presenter welcomes viewers and shows the board."},
    {"start": 45, "end": 140, "title": "Building", "description": "The panels are cut and assembled."},
    {"start": 140, "end": 180, "title": "The finished house", "description": "The birdhouse is shown complete."}
  ],
  "topics": ["woodworking", "birdhouse", "DIY"],
  "onScreenText": [{"text": "Step 1: measure", "firstSeen": 60}],
  "visual": "A tidy home workshop with a desk, a laptop and hand tools; a static camera.",
  "speech": "One presenter speaks in a calm, friendly tone.",
  "language": "en",
  "caveats": []
}`

type summaryHarness struct {
	t       *testing.T
	svc     *VideoSummaryService
	jm      *JobManager
	ollama  *summaryOllama
	leaser  *fakeLeaser
	trace   []string
	traceMu sync.Mutex
	jobID   string
	outDir  string
}

func newSummaryHarness(t *testing.T, probe *models.VideoProbeResponse) *summaryHarness {
	t.Helper()
	h := &summaryHarness{t: t, jm: NewJobManager(), leaser: &fakeLeaser{}}
	h.ollama = newSummaryOllama(t, "vlm-test")
	h.svc = NewVideoSummaryService(VideoSummarySettings{
		OllamaURL: h.ollama.srv.URL, VLMModel: "vlm-test", SummaryModel: "text-test",
		StageTimeout: 10 * time.Second, JobTimeout: 20 * time.Second,
	}, h.jm, nil)
	h.svc.deps.probe = func(context.Context, string) (*models.VideoProbeResponse, error) { return probe, nil }
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) { return []float64{30, 60, 90, 120, 150}, nil }
	h.svc.deps.extractFrames = fakeFrames
	h.svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		return speechTranscript(), nil
	}
	h.svc.deps.leaser = h.leaser
	job := h.jm.CreateJob(models.OriginalFileInfo{Name: "in.mp4", Type: "video/mp4"}, map[string]interface{}{})
	_ = h.jm.SetMode(job.ID, models.VideoSummaryMode)
	_ = h.jm.ReplaceStages(job.ID, BuildVideoSummaryStages(), "queued")
	h.jobID = job.ID
	h.outDir = t.TempDir()
	return h
}

func (h *summaryHarness) run(budget int, lang string) *models.ConversionJob {
	h.t.Helper()
	h.svc.process(context.Background(), VideoSummaryRequest{
		JobID: h.jobID, InputPath: "/in.mp4", OutputDir: h.outDir, OutputLanguage: lang,
		Depth: models.VideoSummaryDepthStandard, MaxFrames: budget,
	}, func(ev string) {
		h.traceMu.Lock()
		h.trace = append(h.trace, ev)
		h.traceMu.Unlock()
	})
	job, err := h.jm.GetJob(h.jobID)
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

func (h *summaryHarness) idx(ev string) int {
	h.traceMu.Lock()
	defer h.traceMu.Unlock()
	for i, e := range h.trace {
		if e == ev {
			return i
		}
	}
	return -1
}

func (h *summaryHarness) readSummary() *models.VideoSummary {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.outDir, SummaryJSONFile))
	if err != nil {
		h.t.Fatal(err)
	}
	var s models.VideoSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		h.t.Fatal(err)
	}
	return &s
}

func videoProbe(duration float64) *models.VideoProbeResponse {
	return &models.VideoProbeResponse{DurationSeconds: duration, Width: 1280, Height: 720, VideoCodec: "h264", HasAudio: true, AudioCodec: "aac"}
}

// ---------------------------------------------------------------------------
// Stage ordering
// ---------------------------------------------------------------------------

func TestVideoSummaryStageOrderingWithFakeScheduler(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	// audio and frames must be able to overlap: each waits (bounded) for the
	// other to have started. Run serially, one of them would time out.
	audioStarted, framesStarted := make(chan struct{}), make(chan struct{})
	overlap := make(chan bool, 2)
	h.svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		close(audioStarted)
		select {
		case <-framesStarted:
			overlap <- true
		case <-time.After(2 * time.Second):
			overlap <- false
		}
		return speechTranscript(), nil
	}
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) {
		close(framesStarted)
		select {
		case <-audioStarted:
			overlap <- true
		case <-time.After(2 * time.Second):
			overlap <- false
		}
		return []float64{30, 60, 90}, nil
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job %s: %s (trace %v)", job.Status, job.Error, h.trace)
	}
	if !<-overlap || !<-overlap {
		t.Fatal("audio and frames did not run in parallel")
	}

	// synthesis starts only after both audio and frames (and vision) are done.
	syn := h.idx("start:synthesis")
	for _, before := range []string{"done:audio", "done:frames", "done:vision", "done:probe"} {
		if i := h.idx(before); i < 0 || i > syn {
			t.Fatalf("%s must precede start:synthesis: %v", before, h.trace)
		}
	}
	if h.idx("start:render") < h.idx("done:synthesis") {
		t.Fatalf("render before synthesis finished: %v", h.trace)
	}

	// stages are reported in order and all completed.
	wantKeys := []string{"probe", "audio", "frames", "vision", "synthesis", "render"}
	if len(job.Stages) != len(wantKeys) {
		t.Fatalf("stages: %+v", job.Stages)
	}
	prev := 0
	for i, st := range job.Stages {
		if st.Key != wantKeys[i] || st.Status != models.StageStatusCompleted {
			t.Fatalf("stage %d = %s/%s, want %s/completed", i, st.Key, st.Status, wantKeys[i])
		}
		if st.Progress <= prev {
			t.Fatalf("stage checkpoints must increase: %+v", job.Stages)
		}
		prev = st.Progress
	}
	if job.Progress != 100 || job.Mode != models.VideoSummaryMode {
		t.Fatalf("progress %d mode %q", job.Progress, job.Mode)
	}

	// TaskWhisper and TaskVLM leases were acquired and released, once each;
	// the Whisper lease was released before synthesis began.
	for _, task := range []gpu.TaskType{gpu.TaskWhisper, gpu.TaskVLM, gpu.TaskOllama} {
		if acq, rel := h.leaser.balanced(task); acq != 1 || rel != 1 {
			t.Fatalf("%s lease acquired %d released %d", task, acq, rel)
		}
	}
	h.leaser.mu.Lock()
	var whisperRel, vlmRel, synAcq time.Time
	for _, e := range h.leaser.events {
		switch {
		case e.task == gpu.TaskWhisper && !e.acquired:
			whisperRel = e.at
		case e.task == gpu.TaskVLM && !e.acquired:
			vlmRel = e.at
		case e.task == gpu.TaskOllama && e.acquired:
			synAcq = e.at
		}
	}
	h.leaser.mu.Unlock()
	if synAcq.Before(whisperRel) || synAcq.Before(vlmRel) {
		t.Fatal("synthesis took the GPU before Whisper and the VLM released it")
	}

	// result wiring
	if job.ResultURL != "/api/summarize/"+h.jobID+"/result?format=md" || job.ResultMime != "text/markdown" || job.ResultFileName != SummaryMarkdownFile {
		t.Fatalf("result fields: url=%q mime=%q name=%q", job.ResultURL, job.ResultMime, job.ResultFileName)
	}
	for _, f := range []string{SummaryJSONFile, SummaryMarkdownFile, SummaryTranscriptFile, SummaryVisionFile} {
		if _, err := os.Stat(filepath.Join(h.outDir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
}

func TestVideoSummaryVisionBatchesUseTranscriptWindow(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) { return nil, nil }
	job := h.run(8, "auto") // 180 s / 8 -> floor 22.5 s, 8 even frames, 2 batches
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if len(h.ollama.vlmCalls) != 2 {
		t.Fatalf("want 2 VLM batches, got %d", len(h.ollama.vlmCalls))
	}
	userOf := func(call map[string]any) string {
		msgs := call["messages"].([]any)
		return msgs[len(msgs)-1].(map[string]any)["content"].(string)
	}
	first, second := userOf(h.ollama.vlmCalls[0]), userOf(h.ollama.vlmCalls[1])
	if !strings.Contains(first, "Hello and welcome.") || !strings.Contains(first, "Today we build") || strings.Contains(first, "finished") {
		t.Fatalf("batch 1 must carry only its window:\n%s", first)
	}
	if !strings.Contains(second, "And it is finished.") || strings.Contains(second, "Hello and welcome.") {
		t.Fatalf("batch 2 must carry only its window:\n%s", second)
	}
	if imgs := h.ollama.vlmCalls[0]["messages"].([]any)[1].(map[string]any)["images"].([]any); len(imgs) != 4 {
		t.Fatalf("batches of 4 frames: %d", len(imgs))
	}
	if h.ollama.vlmCalls[0]["format"] != "json" || h.ollama.vlmCalls[0]["model"] != "vlm-test" {
		t.Fatalf("vlm payload: %v", h.ollama.vlmCalls[0])
	}
}

// ---------------------------------------------------------------------------
// Input paths
// ---------------------------------------------------------------------------

func TestVideoSummaryAudioOnlySkipsFramesAndVision(t *testing.T) {
	probe := &models.VideoProbeResponse{DurationSeconds: 180, HasAudio: true, AudioCodec: "mp3"}
	h := newSummaryHarness(t, probe)
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) {
		t.Error("scene detection must not run for audio")
		return nil, nil
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if len(h.ollama.vlmCalls) != 0 {
		t.Fatalf("no VLM call for audio, got %d", len(h.ollama.vlmCalls))
	}
	byKey := map[string]models.TranscodeStageStatus{}
	for _, st := range job.Stages {
		byKey[st.Key] = st.Status
	}
	if byKey["frames"] != models.StageStatusSkipped || byKey["vision"] != models.StageStatusSkipped || byKey["audio"] != models.StageStatusCompleted {
		t.Fatalf("stages: %v", byKey)
	}
	s := h.readSummary()
	if !hasCaveat(s, CaveatAudioOnly) {
		t.Fatalf("audio-only caveat missing: %v", s.Caveats)
	}
	if s.Visual != "" || len(s.OnScreenText) != 0 {
		t.Fatalf("no picture: visual/onScreenText must be empty: %q %v", s.Visual, s.OnScreenText)
	}
	if acq, _ := h.leaser.balanced(gpu.TaskVLM); acq != 0 {
		t.Fatal("no VLM lease for audio")
	}
	sys := h.ollama.textCalls[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "there is no picture") {
		t.Fatalf("synthesis prompt must say there is no picture:\n%s", sys)
	}
}

func TestVideoSummaryNoSpeechVideoUsesFramesOnly(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		return models.VideoSummaryTranscript{Language: "en"}, nil // music only
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	s := h.readSummary()
	if !hasCaveat(s, CaveatNoSpeech) || hasCaveat(s, CaveatMachineTranscript) {
		t.Fatalf("caveats: %v", s.Caveats)
	}
	if s.Speech != "" {
		t.Fatalf("speech must be empty with no speech: %q", s.Speech)
	}
	for _, st := range job.Stages {
		if st.Key == "audio" && st.Message != "No speech detected" {
			t.Fatalf("audio stage message: %q", st.Message)
		}
	}
}

func TestVideoSummaryVideoWithoutAudioTrackSkipsAudio(t *testing.T) {
	probe := videoProbe(180)
	probe.HasAudio = false
	h := newSummaryHarness(t, probe)
	h.svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		t.Error("whisper must not run without an audio track")
		return models.VideoSummaryTranscript{}, nil
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if job.Stages[1].Key != "audio" || job.Stages[1].Status != models.StageStatusSkipped {
		t.Fatalf("audio stage: %+v", job.Stages[1])
	}
	if s := h.readSummary(); !hasCaveat(s, CaveatNoAudioTrack) {
		t.Fatalf("caveats: %v", s.Caveats)
	}
	if acq, _ := h.leaser.balanced(gpu.TaskWhisper); acq != 0 {
		t.Fatal("no whisper lease without audio")
	}
}

// An MP3 with an attached PNG cover: ffprobe reports a "video" stream with
// attached_pic=1 and r_frame_rate 90000/1. MM73's probe drops it, so the job
// takes the audio-only path and records the caveat.
func TestVideoSummaryCoverArtAudioTakesAudioOnlyPath(t *testing.T) {
	raw := `{"streams":[
	  {"index":0,"codec_type":"audio","codec_name":"mp3","sample_rate":"44100","channels":2,"duration":"180.0"},
	  {"index":1,"codec_type":"video","codec_name":"png","width":600,"height":600,"avg_frame_rate":"0/0","r_frame_rate":"90000/1","disposition":{"attached_pic":1}}
	],"format":{"duration":"180.0","format_name":"mp3"}}`
	report, err := parseVideoProbeReport([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if UsableVideoStream(report) {
		t.Fatalf("cover art must not count as video: %+v", report)
	}
	if !report.HasAudio || report.DurationSeconds != 180 {
		t.Fatalf("probe: %+v", report)
	}
	// Older ffprobe without the disposition flag: the still-image codec rule.
	noFlag := strings.Replace(raw, `,"disposition":{"attached_pic":1}`, "", 1)
	if r2, _ := parseVideoProbeReport([]byte(noFlag)); UsableVideoStream(r2) {
		t.Fatalf("a png stream is never a usable video stream: %+v", r2)
	}

	h := newSummaryHarness(t, report)
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if len(h.ollama.vlmCalls) != 0 {
		t.Fatal("cover art must not reach the VLM")
	}
	if s := h.readSummary(); !hasCaveat(s, CaveatAudioOnly) {
		t.Fatalf("audio-only caveat missing: %v", s.Caveats)
	}
}

func TestVideoSummaryNeitherAudioNorVideoFails(t *testing.T) {
	h := newSummaryHarness(t, &models.VideoProbeResponse{DurationSeconds: 10, Width: 600, Height: 600, VideoCodec: "png"})
	job := h.run(24, "auto")
	if job.Status != models.StatusFailed || !strings.Contains(job.Error, "no audio or video") {
		t.Fatalf("want a clear failure, got %s %q", job.Status, job.Error)
	}
}

// ---------------------------------------------------------------------------
// Synthesis: repair retry, failure, language, deep
// ---------------------------------------------------------------------------

func TestVideoSummarySynthesisRepairRetryThenSuccess(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.ollama.textReply = func(call int) string {
		if call == 1 {
			// chapter past the end of a 180 s video
			return strings.Replace(fixtureSummaryReply, `"end": 180`, `"end": 400`, 1)
		}
		return fixtureSummaryReply
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if len(h.ollama.textCalls) != 2 {
		t.Fatalf("exactly one repair retry: %d calls", len(h.ollama.textCalls))
	}
	repair := h.ollama.textCalls[1]["messages"].([]any)
	last := repair[len(repair)-1].(map[string]any)["content"].(string)
	if !strings.Contains(last, "outside the source duration") {
		t.Fatalf("repair prompt must name the problem: %s", last)
	}
	for _, st := range job.Stages {
		if st.Key == "synthesis" && st.Message != "Repaired once" {
			t.Fatalf("synthesis stage message: %q", st.Message)
		}
	}
}

func TestVideoSummarySynthesisSecondFailureFailsJobClearly(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.ollama.textReply = func(int) string { return `{"title":"x"}` }
	job := h.run(24, "auto")
	if job.Status != models.StatusFailed {
		t.Fatalf("want failed, got %s", job.Status)
	}
	if job.Error != "The summary model returned an invalid summary twice" {
		t.Fatalf("error: %q", job.Error)
	}
	if len(h.ollama.textCalls) != 2 {
		t.Fatalf("no third call: %d", len(h.ollama.textCalls))
	}
	if _, err := os.Stat(filepath.Join(h.outDir, SummaryMarkdownFile)); err == nil {
		t.Fatal("no summary.md on failure")
	}
	for _, st := range job.Stages {
		if st.Key == "synthesis" && st.Status != models.StageStatusFailed {
			t.Fatalf("synthesis stage must be failed: %+v", st)
		}
	}
	if acq, rel := h.leaser.balanced(gpu.TaskOllama); acq != rel {
		t.Fatal("synthesis lease leaked on failure")
	}
}

func TestVideoSummaryOutputLanguageInPromptAndResult(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.run(24, "de-DE")
	sys := h.ollama.textCalls[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "Write every text field in German (de-DE)") {
		t.Fatalf("language rule missing:\n%s", sys)
	}
	if s := h.readSummary(); s.Language != "de-DE" {
		t.Fatalf("language = %q", s.Language)
	}
	// auto follows the speech
	h2 := newSummaryHarness(t, videoProbe(180))
	h2.run(24, "auto")
	sys2 := h2.ollama.textCalls[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys2, "language of the speech (en)") {
		t.Fatalf("auto rule missing:\n%s", sys2)
	}
}

func TestVideoSummaryDeepUsesDeepDaemonOnlyWhenConfigured(t *testing.T) {
	deep := newSummaryOllama(t, "vlm-test")
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.settings.DeepModel = "big-27b"
	h.svc.settings.DeepURL = deep.srv.URL
	h.svc.process(context.Background(), VideoSummaryRequest{JobID: h.jobID, InputPath: "/x", OutputDir: h.outDir, Depth: models.VideoSummaryDepthDeep, MaxFrames: 24}, nil)
	if len(deep.textCalls) != 1 || deep.textCalls[0]["model"] != "big-27b" {
		t.Fatalf("deep synthesis must go to the deep daemon: %d calls", len(deep.textCalls))
	}
	if len(h.ollama.textCalls) != 0 {
		t.Fatal("the standard daemon must not see the synthesis call")
	}
	if len(deep.vlmCalls) != 0 || len(h.ollama.vlmCalls) == 0 {
		t.Fatal("the frame pass stays on the standard daemon (design §7)")
	}
	// standard depth ignores the deep config
	h2 := newSummaryHarness(t, videoProbe(180))
	h2.svc.settings.DeepModel, h2.svc.settings.DeepURL = "big-27b", deep.srv.URL
	h2.run(24, "auto")
	if h2.ollama.textCalls[0]["model"] != "text-test" {
		t.Fatalf("standard depth model: %v", h2.ollama.textCalls[0]["model"])
	}
}

func TestVideoSummaryVisionRepairFailureSkipsBatch(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) { return nil, nil }
	h.ollama.vlmReply = func(call int) string {
		if call <= 2 {
			return "garbage"
		}
		return `{"scene":"ok"}`
	}
	job := h.run(8, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	if s := h.readSummary(); !hasCaveat(s, CaveatSomeFramesSkipped) {
		t.Fatalf("caveat missing: %v", s.Caveats)
	}
}

func TestVideoSummaryOllamaDownFailsJob(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.ollama.srv.Close()
	job := h.run(24, "auto")
	if job.Status != models.StatusFailed || job.Error != "The vision model is not available right now" {
		t.Fatalf("got %s %q", job.Status, job.Error)
	}
	if acq, rel := h.leaser.balanced(gpu.TaskVLM); acq != rel {
		t.Fatal("vlm lease leaked")
	}
}

func TestVideoSummaryWhisperFailureFailsJobAndReleasesLease(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		return models.VideoSummaryTranscript{}, errors.New("cuda oom")
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusFailed || job.Error != "The speech could not be transcribed" {
		t.Fatalf("got %s %q", job.Status, job.Error)
	}
	if acq, rel := h.leaser.balanced(gpu.TaskWhisper); acq != 1 || rel != 1 {
		t.Fatalf("whisper lease %d/%d", acq, rel)
	}
	if strings.Contains(job.Error, "cuda") {
		t.Fatal("raw errors must not reach the user")
	}
}

// A refused or failed job gives its ai_ops unit back (MM73's rule, applied by
// the job manager): the pipeline failing must reach UpdateJobError.
func TestVideoSummaryFailureRefundsQuota(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	refund := &countingRefunder{}
	h.jm.SetJobQuotaCharge(h.jobID, refund)
	h.ollama.textReply = func(int) string { return `nope` }
	h.run(24, "auto")
	if n := refund.n.Load(); n != 1 {
		t.Fatalf("refunds = %d, want 1", n)
	}
}

func TestVideoSummaryQueueSerializesJobs(t *testing.T) {
	jm := NewJobManager()
	ollama := newSummaryOllama(t, "vlm-test")
	svc := NewVideoSummaryService(VideoSummarySettings{OllamaURL: ollama.srv.URL, VLMModel: "vlm-test", SummaryModel: "t", Workers: 1}, jm, nil)
	var mu sync.Mutex
	active, maxActive := 0, 0
	svc.deps.probe = func(context.Context, string) (*models.VideoProbeResponse, error) {
		return &models.VideoProbeResponse{DurationSeconds: 180, HasAudio: true}, nil
	}
	svc.deps.transcribe = func(context.Context, VideoSummaryRequest, string) (models.VideoSummaryTranscript, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return speechTranscript(), nil
	}
	svc.deps.leaser = &fakeLeaser{}
	svc.Start()
	var wg sync.WaitGroup
	var failed sync.Map
	for i := 0; i < 3; i++ {
		job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp3"}, nil)
		_ = jm.ReplaceStages(job.ID, BuildVideoSummaryStages(), "queued")
		// Snapshots from Subscribe are copies; never read the live job pointer
		// while the worker writes it.
		updates := jm.Subscribe(job.ID)
		wg.Add(1)
		go func(id string, ch chan *models.ConversionJob) {
			defer wg.Done()
			timeout := time.After(5 * time.Second)
			for {
				select {
				case snap := <-ch:
					if snap.Status == models.StatusFailed {
						failed.Store(id, snap.Error)
						return
					}
					if snap.Status == models.StatusCompleted {
						return
					}
				case <-timeout:
					failed.Store(id, "timeout")
					return
				}
			}
		}(job.ID, updates)
		svc.Enqueue(VideoSummaryRequest{JobID: job.ID, InputPath: "/a", OutputDir: t.TempDir(), MaxFrames: 24, Depth: "standard"})
	}
	wg.Wait()
	failed.Range(func(k, v any) bool { t.Fatalf("job %v: %v", k, v); return false })
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("SUMMARY_WORKERS=1 must serialise jobs, saw %d at once", maxActive)
	}
}

// ---------------------------------------------------------------------------
// Render
// ---------------------------------------------------------------------------

func hasCaveat(s *models.VideoSummary, c string) bool {
	for _, have := range s.Caveats {
		if have == c {
			return true
		}
	}
	return false
}

// The sample fixture (testdata/video_summary/summary.json) renders to exactly
// testdata/video_summary/summary.md. MM75's "Copy as Markdown" must match it.
func TestRenderSummaryMarkdownFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "video_summary", "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s models.VideoSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "video_summary", "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderSummaryMarkdown(&s); got != string(want) {
		t.Fatalf("summary.md drifted from the fixture:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The fixture is exactly what the pipeline writes for the fixture reply.
func TestVideoSummaryPipelineProducesFixture(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.detectScenes = func(context.Context, string, float64) ([]float64, error) { return []float64{30, 60, 90, 120, 150}, nil }
	if job := h.run(24, "auto"); job.Status != models.StatusCompleted {
		t.Fatalf("job failed: %s", job.Error)
	}
	got, _ := os.ReadFile(filepath.Join(h.outDir, SummaryJSONFile))
	want, err := os.ReadFile(filepath.Join("testdata", "video_summary", "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("summary.json drifted from the fixture:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	md, _ := os.ReadFile(filepath.Join(h.outDir, SummaryMarkdownFile))
	wantMD, _ := os.ReadFile(filepath.Join("testdata", "video_summary", "summary.md"))
	if string(md) != string(wantMD) {
		t.Fatalf("summary.md drifted:\n%s", md)
	}
}

func TestRenderSummaryMarkdownOmitsEmptySections(t *testing.T) {
	s := &models.VideoSummary{Title: "T", Summary: "S."}
	if got := RenderSummaryMarkdown(s); got != "# T\n\nS.\n" {
		t.Fatalf("got %q", got)
	}
}
