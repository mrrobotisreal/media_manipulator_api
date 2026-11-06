package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// fakeRenders is a RunExport ffmpeg runner that parks every render until
// released, so a test can hold the CPU gate full.
type fakeRenders struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	release map[string]chan struct{}
}

func newFakeRenders() *fakeRenders {
	return &fakeRenders{started: map[string]chan struct{}{}, release: map[string]chan struct{}{}}
}

func (f *fakeRenders) ch(m map[string]chan struct{}, id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m[id] == nil {
		m[id] = make(chan struct{})
	}
	return m[id]
}

func (f *fakeRenders) run(ctx context.Context, _ *JobManager, jobID string, _ int, _ float64, _ ...string) error {
	close(f.ch(f.started, jobID))
	select {
	case <-f.ch(f.release, jobID):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// MM119 acceptance: with STUDIO_CPU_ENCODE_CONCURRENCY=2 a third VP9 export
// waits as Queued (currentStage "queued") until a slot frees, its wait lands
// in queueWaitMs and cpuQueueMs; an H.264 export (NVENC) never takes the gate.
func TestStudioCPUEncodeGateQueuesThirdVP9(t *testing.T) {
	// Pin the H.264 encoder to NVENC without probing a GPU.
	studioNVENCOnce.Do(func() {})
	prevNVENC := studioNVENCWorks
	studioNVENCWorks = true
	defer func() { studioNVENCWorks = prevNVENC }()

	jm := NewJobManager()
	fake := newFakeRenders()
	svc := &StudioExportService{cfg: &config.Config{}, jobManager: jm, cpuGate: NewStudioCPUEncodeGate(2), run: fake.run}
	webm, err := ResolveStudioOutput(StudioOutputRequest{Preset: "webm_vp9_opus"})
	if err != nil {
		t.Fatal(err)
	}
	h264, _ := ResolveStudioOutput(StudioOutputRequest{})

	jobs := map[string]*models.ConversionJob{}
	for _, name := range []string{"vp9-a", "vp9-b", "vp9-c", "h264"} {
		jobs[name] = jm.CreateJob(models.OriginalFileInfo{Name: name}, map[string]interface{}{"mode": "studio_export"})
	}
	type result struct {
		stats StudioRenderStats
		err   error
	}
	results := map[string]chan result{}
	start := func(name string, out StudioOutput) {
		plan := depthPlan()
		o := out
		plan.Output = &o
		ch := make(chan result, 1)
		results[name] = ch
		go func() {
			s, err := svc.RunExport(context.Background(), jobs[name].ID, plan, "medium", "/tmp/never-written")
			ch <- result{s, err}
		}()
	}
	started := func(name string) bool {
		select {
		case <-fake.ch(fake.started, jobs[name].ID):
			return true
		default:
			return false
		}
	}
	stage := func(name string) string {
		jm.mu.RLock()
		defer jm.mu.RUnlock()
		return jm.jobs[jobs[name].ID].CurrentStage
	}
	queueWait := func(name string) *int64 {
		jm.mu.RLock()
		defer jm.mu.RUnlock()
		return jm.jobs[jobs[name].ID].QueueWaitMs
	}

	start("vp9-a", webm)
	start("vp9-b", webm)
	waitFor(t, "two VP9 renders", func() bool { return started("vp9-a") && started("vp9-b") })
	start("vp9-c", webm)
	waitFor(t, "the third VP9 to queue", func() bool { return stage("vp9-c") == StudioJobStageQueued })
	if started("vp9-c") {
		t.Fatal("the third VP9 export must wait for a slot")
	}
	// H.264 NVENC never waits, even with the CPU gate full.
	start("h264", h264)
	waitFor(t, "the H.264 render", func() bool { return started("h264") })
	close(fake.ch(fake.release, jobs["h264"].ID))
	if r := <-results["h264"]; r.err != nil || r.stats.CPUEncode || r.stats.CPUQueueMs != 0 || r.stats.Encoder != "nvenc" {
		t.Errorf("h264: %+v %v", r.stats, r.err)
	}
	if w := queueWait("h264"); w != nil {
		t.Errorf("h264 must not record a queue wait, got %d", *w)
	}

	time.Sleep(20 * time.Millisecond) // a measurable wait
	close(fake.ch(fake.release, jobs["vp9-a"].ID))
	waitFor(t, "the third VP9 to start", func() bool { return started("vp9-c") })
	if stage("vp9-c") != "" {
		t.Errorf("the Queued stage must clear once the slot is taken, got %q", stage("vp9-c"))
	}
	close(fake.ch(fake.release, jobs["vp9-b"].ID))
	close(fake.ch(fake.release, jobs["vp9-c"].ID))
	a, c := <-results["vp9-a"], <-results["vp9-c"]
	<-results["vp9-b"]
	if a.err != nil || !a.stats.CPUEncode || a.stats.CPUQueueMs != 0 || a.stats.Encoder != "vpx" {
		t.Errorf("vp9-a: %+v %v", a.stats, a.err)
	}
	if c.err != nil || !c.stats.CPUEncode || c.stats.CPUQueueMs < 15 {
		t.Errorf("vp9-c must report its wait: %+v %v", c.stats, c.err)
	}
	if w := queueWait("vp9-c"); w == nil || *w < 15 {
		t.Errorf("vp9-c queueWaitMs = %v", w)
	}
	if svc.cpuGate.InUse() != 0 {
		t.Errorf("every slot must be released, %d in use", svc.cpuGate.InUse())
	}
}

// A queued export whose job context ends gives up without a slot and fails.
func TestStudioCPUEncodeGateCancelWhileQueued(t *testing.T) {
	g := NewStudioCPUEncodeGate(1)
	rel, _, queued, err := g.Acquire(context.Background(), nil)
	if err != nil || queued {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	called := false
	if _, _, q, err := g.Acquire(ctx, func() { called = true }); err == nil || !q || !called {
		t.Errorf("queued acquire on a dead context: queued=%v called=%v err=%v", q, called, err)
	}
	rel()
	rel() // idempotent
	if g.InUse() != 0 || NewStudioCPUEncodeGate(0).Capacity() != 1 {
		t.Error("release / capacity floor")
	}
}

func TestBuildStudioSRTAndLanguage(t *testing.T) {
	srt := BuildStudioSRT([]models.StudioCaptionCue{
		{ID: "b", StartSeconds: 3.5, EndSeconds: 5, Text: "second"},
		{ID: "a", StartSeconds: 0, EndSeconds: 1.2345, Text: "  first\r\nline  "},
		{ID: "e", StartSeconds: 6, EndSeconds: 7, Text: "   "},
		{ID: "z", StartSeconds: 8, EndSeconds: 8, Text: "zero length"},
		{ID: "h", StartSeconds: 3725.5, EndSeconds: 3726, Text: "an hour in"},
	})
	want := "1\n00:00:00,000 --> 00:00:01,235\nfirst\nline\n\n2\n00:00:03,500 --> 00:00:05,000\nsecond\n\n3\n01:02:05,500 --> 01:02:06,000\nan hour in\n\n"
	if srt != want {
		t.Errorf("srt:\n%q\nwant\n%q", srt, want)
	}
	for in, want := range map[string]string{"en": "eng", "de": "ger", "pt-BR": "por", "zh_Hans": "chi", "FR": "fre", "ger": "ger", "": "", "xx": "", "klingon": ""} {
		if got := StudioSubtitleLanguage(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
	if !strings.Contains(strings.Join(vp9EncodeArgs("high"), " "), "-cpu-used 2 -crf 31") {
		t.Error("vp9 high ladder")
	}
}
