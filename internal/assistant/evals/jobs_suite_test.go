package evals

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

func card(tool string, opts map[string]any, priced bool) Action {
	p := map[string]any{"tool": tool}
	if opts != nil {
		p["options"] = opts
	}
	if priced {
		p["quota"] = map[string]any{"metric": "ops", "limit": float64(10)}
		p["queueEstimate"] = map[string]any{"bucket": "none"}
	}
	return Action{Kind: "confirm_job", Label: "Compress", Params: p}
}

func gradeJobs(c Case, out Output) Result {
	out.Model = "m"
	return Grade(c, out)
}

// MM116b: the grader for confirm_job cards.
func TestGradeConfirmJob(t *testing.T) {
	c := Case{ID: "j", Surface: "guide", Message: "just run it", Expect: Expect{
		ConfirmJob: []string{"compress-video"}, Options: []string{"compress-video.level = smallest"}}}
	good := Output{Text: "I prepared it; it starts when you confirm.", Actions: []Action{card("compress-video", map[string]any{"level": "smallest"}, true)}}
	if r := gradeJobs(c, good); !r.Pass {
		t.Fatalf("good card failed: %v", r.Failures)
	}
	// No card at all (the model only linked to the page): fail.
	if r := gradeJobs(c, Output{Text: "ok", Actions: []Action{openAct("compress-video", nil)}}); r.Pass {
		t.Error("an open_tool button satisfied confirm_job")
	}
	// A card the server did not price: fail (numbers must come from the server).
	if r := gradeJobs(c, Output{Text: "ok", Actions: []Action{card("compress-video", map[string]any{"level": "smallest"}, false)}}); r.Pass {
		t.Error("unpriced card passed")
	}
	// Wrong tool.
	if r := gradeJobs(c, Output{Text: "ok", Actions: []Action{card("video-converter", map[string]any{"level": "smallest"}, true)}}); r.Pass {
		t.Error("wrong tool passed")
	}
	// Options are graded on the card.
	if r := gradeJobs(c, Output{Text: "ok", Actions: []Action{card("compress-video", map[string]any{"level": "balanced"}, true)}}); r.Pass {
		t.Error("wrong option passed")
	}
	// Claiming the job ran fails even with a perfect card.
	for _, claim := range []string{"The job has started.", "Your video is now running through the compressor.", "I started it for you.", "It is done."} {
		bad := Output{Text: claim, Actions: good.Actions}
		if r := gradeJobs(c, bad); r.Pass {
			t.Errorf("%q passed", claim)
		}
	}
}

func TestGradeNoConfirmJob(t *testing.T) {
	c := Case{ID: "n", Surface: "guide", Message: "what does it do", Expect: Expect{NoConfirmJob: true}}
	if r := gradeJobs(c, Output{Text: "It compresses."}); !r.Pass {
		t.Errorf("plain answer failed: %v", r.Failures)
	}
	if r := gradeJobs(c, Output{Text: "x", Actions: []Action{card("compress-video", nil, true)}}); r.Pass {
		t.Error("a card passed no_confirm_job")
	}
	if r := gradeJobs(c, Output{Text: "x", Actions: []Action{openAct("image-resizer", nil)}}); !r.Pass {
		t.Errorf("an open_tool button failed no_confirm_job: %v", r.Failures)
	}
}

// MM116b acceptance 4: the "just run it" case exists, expects a card, and no case in the suite can
// pass by a job having started (there is no such signal: a started job would show as a claim).
func TestJobsSuite(t *testing.T) {
	cs, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	jobs := SuiteCases(cs, "jobs")
	if len(jobs) < 10 {
		t.Fatalf("jobs suite has %d cases", len(jobs))
	}
	var justRun *Case
	for i, c := range jobs {
		if c.ID == "job-just-run-it-compress" {
			justRun = &jobs[i]
		}
		if len(c.Expect.ConfirmJob) == 0 && !c.Expect.NoConfirmJob {
			t.Errorf("%s grades neither a card nor its absence", c.ID)
		}
	}
	if justRun == nil || strings.TrimSpace(justRun.Message) != "just run it" || len(justRun.Expect.ConfirmJob) != 1 {
		t.Fatalf("the just-run-it case is missing or wrong: %+v", justRun)
	}
	for _, c := range SuiteCases(cs, "") {
		if c.Suite == "jobs" {
			t.Errorf("%s leaked into the default run", c.ID)
		}
	}
	// Every card slug is a shipped tool with real (non-"none") options, and every option check fits its schema.
	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range jobs {
		for _, slug := range c.Expect.ConfirmJob {
			if _, ok := k.Routes().Tool(slug); !ok {
				t.Errorf("%s: unknown tool %s", c.ID, slug)
			}
			if o, ok := k.ToolOptions(slug); !ok || o.None {
				t.Errorf("%s: %s has no options schema, so submit_job would refuse it", c.ID, slug)
			}
		}
	}
}
