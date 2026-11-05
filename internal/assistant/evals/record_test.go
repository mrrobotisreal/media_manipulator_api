package evals

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type fakeRunStore struct {
	rows []telemetry.AssistantEvalRun
}

func (f *fakeRunStore) InsertAssistantEvalRun(_ context.Context, r telemetry.AssistantEvalRun) error {
	f.rows = append(f.rows, r)
	return nil
}

func (f *fakeRunStore) LatestAssistantEvalRun(_ context.Context, surface string) (*telemetry.AssistantEvalRun, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].Surface == surface {
			r := f.rows[i]
			return &r, nil
		}
	}
	return nil, nil
}

func res(surface string, pass bool, skipped string, calls int, cost float64) Result {
	return Result{Case: Case{ID: "x", Surface: surface}, Pass: pass, Skipped: skipped, Output: Output{ToolCalls: calls, CostUSD: cost}}
}

func TestRecordsPerSurface(t *testing.T) {
	rows := Records([]Result{
		res("guide", true, "", 1, 0.01), res("guide", false, "", 3, 0.02), res("guide", true, "", 5, 0.01),
		res("photo", true, "", 2, 0.05), res("studio", false, "requires studio", 0, 0),
	}, RunMeta{PromptVersion: "abc123def456", GitSHA: "deadbee", ModelGuide: "g", ModelCopilot: "c", EvidencePath: "/tmp/e.md"})
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	g := rows[0]
	if g.Surface != "guide" || g.Cases != 3 || g.Passed != 2 || g.Failed != 1 || g.MedianToolCalls != 3 || g.PromptVersion != "abc123def456" || g.EvidencePath != "/tmp/e.md" {
		t.Errorf("guide row = %+v", g)
	}
	if d := g.CostUSD - 0.04; d > 1e-9 || d < -1e-9 {
		t.Errorf("guide cost = %v", g.CostUSD)
	}
	if s := rows[2]; s.Surface != "studio" || s.Skipped != 1 || s.Passed+s.Failed != 0 {
		t.Errorf("studio row = %+v", s)
	}
}

// MM113 acceptance 4: the gate compares with the latest recorded run per surface.
func TestGateAgainstFakePreviousRuns(t *testing.T) {
	st := &fakeRunStore{rows: []telemetry.AssistantEvalRun{
		{Surface: "guide", PromptVersion: "old000000001", Passed: 18, Failed: 2, CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{Surface: "guide", PromptVersion: "old000000002", Passed: 9, Failed: 1, CreatedAt: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		{Surface: "photo", PromptVersion: "old000000002", Passed: 4, Failed: 0},
	}}
	cur := []telemetry.AssistantEvalRun{
		{Surface: "guide", Passed: 17, Failed: 3}, // 85 % < 90 % latest: regression
		{Surface: "photo", Passed: 8, Failed: 0},  // equal rate: ok
		{Surface: "studio", Passed: 1, Failed: 9}, // no earlier run: ok
		{Surface: "vector", Skipped: 4},           // nothing ran: ok
	}
	vs, err := Gate(context.Background(), st, cur)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"guide": true, "photo": false, "studio": false, "vector": false}
	for _, v := range vs {
		if v.Regress != want[v.Surface] {
			t.Errorf("%s regress = %v, want %v", v.Surface, v.Regress, want[v.Surface])
		}
	}
	if !Regressed(vs) {
		t.Error("Regressed should be true")
	}
	if line := vs[0].Line(); !strings.Contains(line, "old000000002") || !strings.Contains(line, "REGRESSION") {
		t.Errorf("line names the previous prompt version: %q", line)
	}
	if line := vs[2].Line(); !strings.Contains(line, "no earlier recorded run") {
		t.Errorf("line = %q", line)
	}
	// Recording after the gate adds rows the next gate reads.
	if err := Record(context.Background(), st, cur[:1]); err != nil || len(st.rows) != 4 {
		t.Fatalf("record: %v %d", err, len(st.rows))
	}
	vs, _ = Gate(context.Background(), st, []telemetry.AssistantEvalRun{{Surface: "guide", Passed: 17, Failed: 3}})
	if vs[0].Regress {
		t.Error("equal to the newly recorded run is not a regression")
	}
}
