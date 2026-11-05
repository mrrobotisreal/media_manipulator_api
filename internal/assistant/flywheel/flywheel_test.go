package flywheel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/evals"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) // 2026-W46

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type fakeSource struct{ rows []MessageRow }

func (f fakeSource) Messages(_ context.Context, from, to time.Time) ([]MessageRow, error) {
	return f.rows, nil
}

// fakeRetriever maps a keyword in the query to doc ids.
type fakeRetriever struct{ m map[string][]string }

func (f fakeRetriever) Search(q, surface string, k int) []assistant.Hit {
	var out []assistant.Hit
	for kw, ids := range f.m {
		if strings.Contains(strings.ToLower(q), kw) {
			for _, id := range ids {
				out = append(out, assistant.Hit{Doc: assistant.Doc{ID: id}})
			}
		}
	}
	if len(out) > k {
		out = out[:k]
	}
	return out
}
func (f fakeRetriever) Get(string) (assistant.Doc, bool) { return assistant.Doc{}, false }

type conv struct {
	id, surface string
	seq         int
	rows        []MessageRow
}

// turn appends a user+assistant pair.
func (c *conv) turn(at time.Time, user, reply, rating, errCode string, calls ...map[string]string) {
	tid := fmt.Sprintf("%s-t%d", c.id, c.seq)
	c.seq++
	c.rows = append(c.rows, MessageRow{ID: tid + "-u", ConversationID: c.id, Surface: c.surface, Seq: c.seq*2 - 1, TurnID: tid, Role: "user",
		Content: js(map[string]any{"text": user, "page": "/tools"}), Status: "complete", CreatedAt: at})
	var tcs []map[string]string
	for _, cl := range calls {
		tcs = append(tcs, cl)
	}
	c.rows = append(c.rows, MessageRow{ID: tid + "-a", ConversationID: c.id, Surface: c.surface, Seq: c.seq * 2, TurnID: tid, Role: "assistant",
		Content: js(map[string]any{"text": reply, "error": errCode, "toolCalls": tcs}), Status: "complete", PromptVersion: "abc123def456",
		CreatedAt: at.Add(3 * time.Second), Rating: rating})
}

func call(name, args string) map[string]string {
	return map[string]string{"name": name, "arguments": args}
}

func fixtureRows() []MessageRow {
	var rows []MessageRow
	// A: two guide users both thumbs-down on "gif" asks (same cluster).
	for i, u := range []string{"make a gif from my clip", "how to turn video into a gif"} {
		c := &conv{id: fmt.Sprintf("a%d", i), surface: "guide"}
		c.turn(t0.Add(time.Duration(i)*time.Hour), u, "Use the converter.", "down", "", call("open_tool", `{"slug":"convert-video-to-animated-gif"}`))
		rows = append(rows, c.rows...)
	}
	// B: invalid_ops on photo "moody".
	b := &conv{id: "b", surface: "photo"}
	b.turn(t0.Add(2*time.Hour), "make it moody", "", "", assistant.CodeInvalid)
	rows = append(rows, b.rows...)
	// C: out-of-scope then rephrase 2 minutes later.
	c := &conv{id: "c", surface: "guide"}
	c.turn(t0.Add(3*time.Hour), "write me a poem about cats", assistant.OutOfScopeLine("en"), "", "")
	c.turn(t0.Add(3*time.Hour+2*time.Minute), "ok remove background of my cat photo", "Open the background remover.", "up", "", call("open_tool", `{"slug":"remove-background-from-image"}`))
	rows = append(rows, c.rows...)
	// D: out-of-scope with no follow-up: not flagged.
	d := &conv{id: "d", surface: "guide"}
	d.turn(t0.Add(4*time.Hour), "who won the game", assistant.OutOfScopeLine("en"), "", "")
	rows = append(rows, d.rows...)
	// E: guide answer with no button, user asks again 1 minute later.
	e := &conv{id: "e", surface: "guide"}
	e.turn(t0.Add(5*time.Hour), "my audio has hiss", "Hiss is high-frequency noise.", "", "")
	e.turn(t0.Add(5*time.Hour+time.Minute), "so which tool?", "Try Remove noise.", "", "", call("open_tool", `{"slug":"remove-noise-from-audio"}`))
	rows = append(rows, e.rows...)
	// F: a fine turn.
	f := &conv{id: "f", surface: "guide"}
	f.turn(t0.Add(6*time.Hour), "compress my video", "Here.", "up", "", call("open_tool", `{"slug":"compress-video"}`))
	rows = append(rows, f.rows...)
	// G: outside the week.
	g := &conv{id: "g", surface: "guide"}
	g.turn(t0.AddDate(0, 0, 14), "old thumbs down", "x", "down", "")
	rows = append(rows, g.rows...)
	return rows
}

func testRetriever() fakeRetriever {
	return fakeRetriever{m: map[string][]string{
		"gif": {"tools/convert-video-to-animated-gif"}, "moody": {"recipes/photo/bw-classic"},
		"background": {"tools/remove-background-from-image"}, "poem": nil, "audio": {"tools/remove-noise-from-audio"},
	}}
}

func week(t *testing.T) Week {
	w, err := ParseWeek("2026-W46", t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestParseWeek(t *testing.T) {
	w := week(t)
	if w.From != time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) || w.To != time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("W46 = %v..%v", w.From, w.To)
	}
	last, err := ParseWeek("", time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	if err != nil || last.Label != "2026-W46" {
		t.Fatalf("default week = %v %v", last.Label, err)
	}
	for _, bad := range []string{"2026-46", "2026-W00", "2026-W54", "x"} {
		if _, err := ParseWeek(bad, t0); err == nil {
			t.Errorf("ParseWeek(%q) accepted", bad)
		}
	}
}

func TestSelectFlagsEachServerSignal(t *testing.T) {
	w := week(t)
	got := Select(BuildTurns(fixtureRows()), nil, w.From, w.To)
	by := map[string][]Signal{}
	for _, f := range got {
		by[f.Turn.ConversationID] = append(by[f.Turn.ConversationID], f.Signals...)
	}
	want := map[string]Signal{"a0": SigThumbsDown, "a1": SigThumbsDown, "b": SigInvalidOps, "c": SigOOSRephrased, "e": SigNoRoute}
	for conv, s := range want {
		found := false
		for _, x := range by[conv] {
			found = found || x == s
		}
		if !found {
			t.Errorf("conversation %s missing %s: %v", conv, s, by[conv])
		}
	}
	for _, clean := range []string{"d", "f", "g"} {
		if len(by[clean]) != 0 {
			t.Errorf("conversation %s must not be flagged: %v", clean, by[clean])
		}
	}
}

func TestSelectUsesAnalyticsExport(t *testing.T) {
	rows := fixtureRows()
	an, err := ParseAnalytics(strings.NewReader(`{"signal":"undo_within_60s","turn_id":"f-t0"}` + "\n\n" + `{"signal":"guide_no_click","message_id":"d-t0-a"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := week(t)
	got := Select(BuildTurns(rows), an, w.From, w.To)
	seen := map[string]Signal{}
	for _, f := range got {
		if f.Turn.ConversationID == "f" || f.Turn.ConversationID == "d" {
			seen[f.Turn.ConversationID] = f.Signals[0]
		}
	}
	if seen["f"] != SigFastUndo || seen["d"] != SigNoClick {
		t.Fatalf("analytics join: %v", seen)
	}
	for _, bad := range []string{`{"signal":"thumbs_down","turn_id":"x"}`, `{"signal":"undo_within_60s"}`, `not json`} {
		if _, err := ParseAnalytics(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestClusterGroupsBySurfaceAndDocs(t *testing.T) {
	w := week(t)
	fl := Select(BuildTurns(fixtureRows()), nil, w.From, w.To)
	cl := ClusterTurns(fl, testRetriever(), 2)
	if len(cl) < 4 {
		t.Fatalf("clusters = %d", len(cl))
	}
	top := cl[0]
	if top.Key != "guide | tools/convert-video-to-animated-gif" || top.Size() != 2 || top.Counts[SigThumbsDown] != 2 {
		t.Fatalf("top cluster = %q size %d counts %v", top.Key, top.Size(), top.Counts)
	}
	if top.Consulted["tools/convert-video-to-animated-gif"] != 2 {
		t.Fatalf("consulted = %v", top.Consulted)
	}
	keys := map[string]bool{}
	for _, c := range cl {
		keys[c.Key] = true
	}
	for _, k := range []string{"photo | recipes/photo/bw-classic", "guide | " + NoDocs} {
		if !keys[k] {
			t.Errorf("missing cluster %q in %v", k, keys)
		}
	}
	// Deterministic.
	again := ClusterTurns(fl, testRetriever(), 2)
	for i := range cl {
		if cl[i].Key != again[i].Key {
			t.Fatalf("order not deterministic at %d", i)
		}
	}
}

func TestScrubRemovesPII(t *testing.T) {
	cases := []struct{ in, absent string }{
		{"mail me at jo.park+x@example.co.uk please", "example.co.uk"},
		{"I uploaded holiday_2024_final.mp4 and it failed", "holiday_2024"},
		{"the file 'My Wedding Video.mov' is huge", "Wedding"},
		{"see C:\\Users\\jo\\Videos\\clip.avi now", "Users"},
		{"it is at /home/jo/videos/clip and /Users/jo/x", "jo/videos"},
		{"look at https://evil.example.com/a?token=SECRET1", "evil.example"},
		{"also www.example.org/page", "example.org"},
		{"call +1 (555) 123-4567 tomorrow", "555"},
		{"call 07911 123456", "123456"},
		{"order 12345678901234 failed", "12345678901234"},
		{"server 192.168.1.20 down", "192.168"},
		{"IMG_0042.HEIC will not open", "IMG_0042"},
	}
	for _, c := range cases {
		got := Scrub(c.in)
		if strings.Contains(got, c.absent) {
			t.Errorf("Scrub(%q) = %q still has %q", c.in, got, c.absent)
		}
	}
	keep := Scrub("read https://media-manipulator.com/tools/compress-video?utm=abc#x or go to /pricing, 4K at 60 fps")
	for _, s := range []string{"https://media-manipulator.com/tools/compress-video", "/pricing", "4K at 60 fps"} {
		if !strings.Contains(keep, s) {
			t.Errorf("Scrub dropped %q: %q", s, keep)
		}
	}
	if strings.Contains(keep, "utm") {
		t.Errorf("query string kept: %q", keep)
	}
	if got := Scrub("https://media-manipulator.com.evil.io/x"); strings.Contains(got, "evil") {
		t.Errorf("lookalike origin kept: %q", got)
	}
}

func TestExampleTruncatesAfterScrubbing(t *testing.T) {
	long := strings.Repeat("compress my video ", 40) + " write to a@b.co"
	got := Example(long)
	if n := len([]rune(got)); n > ExampleMax {
		t.Fatalf("len = %d", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("no ellipsis: %q", got)
	}
	// An email straddling the cut is removed before the cut, not half-kept.
	edge := strings.Repeat("x ", 97) + "me@example.com"
	if got := Example(edge); strings.Contains(got, "@") || strings.Contains(got, "example") {
		t.Fatalf("half email survived: %q", got)
	}
	// Unicode survives the cut intact.
	ru := Example(strings.Repeat("привет ", 60))
	if !strings.HasPrefix(ru, "привет") || len([]rune(ru)) > ExampleMax {
		t.Fatalf("unicode cut: %q", ru)
	}
}

func buildReport(t *testing.T, label bool) Report {
	w := week(t)
	fl := Select(BuildTurns(fixtureRows()), nil, w.From, w.To)
	cl := ClusterTurns(fl, testRetriever(), 2)
	if label {
		for _, c := range cl {
			c.Label = "gif asks"
		}
	}
	return Report{Week: w, Generated: t0, TurnsInWeek: 9, Flagged: len(fl), Clusters: cl, SignalGaps: []Signal{SigFastUndo, SigNoClick}, Labeled: label}
}

func TestRenderReport(t *testing.T) {
	rows := fixtureRows()
	// A turn with PII and a very long message.
	rows = append(rows, (&conv{id: "z", surface: "guide"}).withTurn(t0.Add(time.Hour), "email jo@example.com about C:\\vids\\a.mp4 "+strings.Repeat("blah ", 100), "ok", "down")...)
	w := week(t)
	fl := Select(BuildTurns(rows), nil, w.From, w.To)
	cl := ClusterTurns(fl, testRetriever(), 2)
	md := Render(Report{Week: w, Generated: t0, TurnsInWeek: 10, Flagged: len(fl), Clusters: cl, SignalGaps: []Signal{SigFastUndo}})
	for _, s := range []string{"# Medi flywheel, 2026-W46", "## Summary", "## Cluster 1: guide | tools/convert-video-to-animated-gif",
		"whenToUse sidecar fix", "new recipe stub", "```yaml", "- id: flywheel-2026w46-01", "surface: guide", "actions: [tool:convert-video-to-animated-gif]",
		"Haiku labelling: off", "`undo_within_60s`", "Docs the model read or opened"} {
		if !strings.Contains(md, s) {
			t.Errorf("report missing %q", s)
		}
	}
	for _, s := range []string{"jo@example.com", "a.mp4", "C:\\vids"} {
		if strings.Contains(md, s) {
			t.Errorf("report leaks %q", s)
		}
	}
	for _, line := range strings.Split(md, "\n") {
		if i := strings.Index(line, "user: \""); i >= 0 {
			if n := len([]rune(strings.TrimSuffix(line[i+7:], "\""))); n > ExampleMax {
				t.Errorf("example %d runes: %.60s", n, line)
			}
		}
	}
	if !strings.Contains(Render(buildReport(t, true)), "Label: **gif asks**") {
		t.Error("label missing")
	}
	if !strings.Contains(Render(Report{Week: w, Generated: t0}), "No flagged turns this week.") {
		t.Error("empty report")
	}
}

func (c *conv) withTurn(at time.Time, user, reply, rating string) []MessageRow {
	c.turn(at, user, reply, rating, "")
	return c.rows
}

// The proposed eval case must load with the real loader (so it is truly paste-ready).
func TestProposedEvalCaseParses(t *testing.T) {
	rep := buildReport(t, false)
	if len(rep.Clusters) == 0 {
		t.Fatal("no clusters")
	}
	for i, c := range rep.Clusters {
		p := Propose(c, rep.Week.Label, i+1)
		if p.Kind == "" || p.Target == "" || p.Body == "" {
			t.Fatalf("empty proposal for %q: %+v", c.Key, p)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(p.EvalCase), 0o600); err != nil {
			t.Fatal(err)
		}
		cases, err := evals.LoadCasesFrom(os.DirFS(dir), ".")
		if err != nil {
			t.Fatalf("cluster %q: eval YAML does not load: %v\n%s", c.Key, err, p.EvalCase)
		}
		if len(cases) != 1 || cases[0].Surface != c.Surface {
			t.Fatalf("cluster %q: case %+v", c.Key, cases)
		}
	}
}

func TestProposalKinds(t *testing.T) {
	rep := buildReport(t, false)
	kinds := map[string]ProposalKind{}
	for i, c := range rep.Clusters {
		kinds[c.Key] = Propose(c, rep.Week.Label, i+1).Kind
	}
	if kinds["guide | tools/convert-video-to-animated-gif"] != ProposeWhenToUse {
		t.Errorf("tool cluster: %v", kinds)
	}
	if kinds["photo | recipes/photo/bw-classic"] != ProposeRecipe || kinds["guide | "+NoDocs] != ProposeRecipe {
		t.Errorf("recipe clusters: %v", kinds)
	}
}

// --- read-only guarantee ---

func TestEveryQueryIsReadOnly(t *testing.T) {
	if len(Queries) == 0 {
		t.Fatal("no queries")
	}
	for _, q := range Queries {
		if err := CheckReadOnly(q); err != nil {
			t.Errorf("%v\n%s", err, q)
		}
	}
	for _, bad := range []string{"DELETE FROM assistant_messages", "UPDATE assistant_messages SET role='x'", "SELECT 1; DROP TABLE x",
		"INSERT INTO t VALUES (1)", "WITH x AS (DELETE FROM t RETURNING 1) SELECT * FROM x", "SELECT 1 FOR UPDATE; COPY t TO '/x'", "TRUNCATE t", ""} {
		if CheckReadOnly(bad) == nil {
			t.Errorf("CheckReadOnly accepted %q", bad)
		}
	}
	if strings.Contains(QueryMessages, "digest)") || !strings.Contains(QueryMessages, "- 'digest'") {
		t.Error("the digest must be stripped in SQL")
	}
	if !strings.Contains(QueryMessages, "deleted_at IS NULL") {
		t.Error("deleted conversations must be excluded")
	}
}

// The only database surface is Querier, which has no Exec: a compile-time pin.
var _ Querier = (pgx.Tx)(nil)

type recordingQuerier struct{ sqls []string }

func (r *recordingQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	r.sqls = append(r.sqls, sql)
	return nil, io.EOF
}

func TestPGSourceOnlyRunsCheckedSelects(t *testing.T) {
	q := &recordingQuerier{}
	_, _ = PGSource{Q: q}.Messages(context.Background(), t0, t0.Add(time.Hour))
	if len(q.sqls) != 1 {
		t.Fatalf("queries = %d", len(q.sqls))
	}
	if err := CheckReadOnly(q.sqls[0]); err != nil {
		t.Fatal(err)
	}
}

func TestNoWriteCallsInPackage(t *testing.T) {
	for _, dir := range []string{".", "../../../cmd/assistant-flywheel"} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			for _, banned := range []string{".Exec(", "CopyFrom(", "SendBatch(", "Commit(", "telemetry.NewStore", "AppendAssistant", "SetAssistant"} {
				if bytes.Contains(b, []byte(banned)) {
					t.Errorf("%s/%s uses %s", dir, e.Name(), banned)
				}
			}
		}
	}
}

// --- flags ---

func TestParseFlags(t *testing.T) {
	ok := func(args ...string) Options {
		o, err := ParseFlags(args, io.Discard)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return o
	}
	bad := func(args ...string) {
		if _, err := ParseFlags(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	o := ok()
	if o.Label || o.Estimate || o.Depth != 2 || o.MaxClusters != 15 || o.MaxUSD != 0.25 {
		t.Fatalf("defaults = %+v", o)
	}
	o = ok("--week", "2026-W46", "--env-file", "/x/.env", "--estimate", "--label", "--export-routing", "/tmp/r.jsonl", "--analytics-export", "/tmp/a.jsonl")
	if o.Week != "2026-W46" || !o.Estimate || !o.Label || o.ExportRouting != "/tmp/r.jsonl" || o.AnalyticsExport != "/tmp/a.jsonl" {
		t.Fatalf("parsed = %+v", o)
	}
	ok("--label", "--i-know-this-costs-money")
	bad("--label")                   // costs money without the confirmation
	bad("--i-know-this-costs-money") // confirmation without --label
	bad("--depth", "0")              // out of range
	bad("--max-clusters", "0")
	bad("--max-usd", "0")
	bad("stray")
	bad("--nope")
}

// --- Run, label, routing export ---

type fakeProvider struct {
	calls int
	err   error
}

func (f *fakeProvider) Stream(_ context.Context, req assistant.Request) (assistant.Stream, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &fakeStream{evs: []assistant.Event{
		{Kind: assistant.EventDelta, Text: "GIF conversion asks\nignored"},
		{Kind: assistant.EventUsage, Usage: &assistant.Usage{InputTokens: 300, OutputTokens: 8, CostUSD: 0.0004, Reported: true}},
		{Kind: assistant.EventDone},
	}}, nil
}

type fakeStream struct {
	evs []assistant.Event
	i   int
}

func (s *fakeStream) Next() (assistant.Event, error) {
	if s.i >= len(s.evs) {
		return assistant.Event{}, io.EOF
	}
	s.i++
	return s.evs[s.i-1], nil
}
func (s *fakeStream) Close() error { return nil }

func runWith(t *testing.T, o Options, prov *fakeProvider) (map[string]string, *Report, string, error) {
	files := map[string]string{}
	var out bytes.Buffer
	d := Deps{Source: fakeSource{rows: fixtureRows()}, Retriever: testRetriever(), LabelModel: "anthropic/claude-haiku-5.5",
		Now: func() time.Time { return t0.AddDate(0, 0, 7) }, Stdout: &out,
		WriteFile: func(p string, b []byte) error { files[p] = string(b); return nil }}
	if prov != nil && o.Label && !o.Estimate {
		d.Labeler = &Labeler{Provider: prov, Model: d.LabelModel}
	}
	rep, err := Run(context.Background(), o, d)
	return files, rep, out.String(), err
}

func TestRunWritesReportAndNothingElse(t *testing.T) {
	files, rep, out, err := runWith(t, Options{Depth: 2, MaxClusters: 15, MaxUSD: 0.25}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["docs/evidence/medi-flywheel-2026-W46.md"]; !ok || len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if rep.Flagged == 0 || !strings.Contains(out, "report: docs/evidence/medi-flywheel-2026-W46.md") {
		t.Fatalf("out = %s", out)
	}
}

func TestRunEstimateWritesNothingAndCallsNoModel(t *testing.T) {
	prov := &fakeProvider{}
	files, _, out, err := runWith(t, Options{Depth: 2, MaxClusters: 15, MaxUSD: 0.25, Estimate: true, Label: true}, prov)
	if err != nil || len(files) != 0 || prov.calls != 0 {
		t.Fatalf("files=%v calls=%d err=%v", files, prov.calls, err)
	}
	if !strings.Contains(out, "label estimate:") {
		t.Fatalf("no estimate printed: %s", out)
	}
}

func TestRunLabelsWithinBudget(t *testing.T) {
	prov := &fakeProvider{}
	files, rep, _, err := runWith(t, Options{Depth: 2, MaxClusters: 15, MaxUSD: 0.25, Label: true, Confirm: true, Week: "2026-W46"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != len(rep.Clusters) || !rep.Labeled || rep.LabelUSD <= 0 {
		t.Fatalf("calls=%d clusters=%d labeled=%v usd=%v", prov.calls, len(rep.Clusters), rep.Labeled, rep.LabelUSD)
	}
	if md := files["docs/evidence/medi-flywheel-2026-W46.md"]; !strings.Contains(md, "Label: **GIF conversion asks**") || !strings.Contains(md, "Haiku labelling: on") {
		t.Fatalf("report: %.300s", md)
	}
	// Over the cap: refuses before any call.
	prov2 := &fakeProvider{}
	_, _, _, err = runWith(t, Options{Depth: 2, MaxClusters: 15, MaxUSD: 0.0000001, Label: true, Confirm: true}, prov2)
	if err == nil || prov2.calls != 0 {
		t.Fatalf("expected a budget refusal, err=%v calls=%d", err, prov2.calls)
	}
}

func TestLabelPromptIsScrubbed(t *testing.T) {
	rep := buildReport(t, false)
	rows := (&conv{id: "p", surface: "guide"}).withTurn(t0.Add(time.Hour), "gif from jo@example.com C:\\a\\b.mp4", "x", "down")
	fl := Select(BuildTurns(rows), nil, rep.Week.From, rep.Week.To)
	cl := ClusterTurns(fl, testRetriever(), 2)
	p := labelPrompt(cl[0])
	if strings.Contains(p, "example.com") || strings.Contains(p, "b.mp4") {
		t.Fatalf("label prompt leaks: %s", p)
	}
}

func TestExportRoutingGradesPairs(t *testing.T) {
	files, _, _, err := runWith(t, Options{Depth: 2, MaxClusters: 15, MaxUSD: 0.25, ExportRouting: "/tmp/routing.jsonl"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []RoutingPair
	for _, l := range strings.Split(strings.TrimSpace(files["/tmp/routing.jsonl"]), "\n") {
		var p RoutingPair
		if err := json.Unmarshal([]byte(l), &p); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		pairs = append(pairs, p)
	}
	grade := map[string]RoutingPair{}
	for _, p := range pairs {
		grade[p.Message] = p
	}
	if p := grade["make a gif from my clip"]; p.Grade != "bad" || p.Targets[0] != "convert-video-to-animated-gif" || p.GradedBy[0] != "thumbs_down" {
		t.Errorf("thumbs-down pair: %+v", p)
	}
	if p := grade["ok remove background of my cat photo"]; p.Grade != "good" {
		t.Errorf("thumbs-up pair: %+v", p)
	}
	if p := grade["compress my video"]; p.Grade != "good" {
		t.Errorf("good pair: %+v", p)
	}
	if _, ok := grade["write me a poem about cats"]; ok {
		t.Error("a turn with no route must not be exported")
	}
	if _, ok := grade["old thumbs down"]; ok {
		t.Error("a turn outside the week must not be exported")
	}
}

func TestRunSourceErrorIsReported(t *testing.T) {
	d := Deps{Source: errSource{}, Retriever: testRetriever()}
	if _, err := Run(context.Background(), Options{Depth: 2, MaxClusters: 1, MaxUSD: 1, Week: "2026-W46"}, d); err == nil {
		t.Fatal("expected an error")
	}
}

type errSource struct{}

func (errSource) Messages(context.Context, time.Time, time.Time) ([]MessageRow, error) {
	return nil, io.ErrUnexpectedEOF
}
