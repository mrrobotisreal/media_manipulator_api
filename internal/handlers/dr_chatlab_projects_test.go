package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Pure unit tests for the chat-lab Projects helpers — no DB, no network, no S3.

// ---- projectAssetKind ---------------------------------------------------------

func TestProjectAssetKind(t *testing.T) {
	cases := []struct {
		name        string
		fileName    string
		contentType string
		wantKind    string
		wantExt     string
		wantMax     int64
		wantOK      bool
	}{
		// text — extension-first, MIME irrelevant
		{"md", "notes.md", "text/markdown", "text", "md", drChatLabMaxCodeTextAssetBytes, true},
		{"txt", "notes.txt", "text/plain", "text", "txt", drChatLabMaxCodeTextAssetBytes, true},
		{"csv octet-stream", "data.csv", "application/octet-stream", "text", "csv", drChatLabMaxCodeTextAssetBytes, true},
		{"json", "config.json", "application/json", "text", "json", drChatLabMaxCodeTextAssetBytes, true},
		{"yaml", "ci.yaml", "", "text", "yaml", drChatLabMaxCodeTextAssetBytes, true},
		{"yml", "ci.yml", "", "text", "yml", drChatLabMaxCodeTextAssetBytes, true},
		{"toml", "cfg.toml", "", "text", "toml", drChatLabMaxCodeTextAssetBytes, true},
		{"xml", "feed.xml", "text/xml", "text", "xml", drChatLabMaxCodeTextAssetBytes, true},
		// code — exotic browser MIME types must not matter
		{"go with text/x-go", "main.go", "text/x-go", "code", "go", drChatLabMaxCodeTextAssetBytes, true},
		{"go with empty type", "main.go", "", "code", "go", drChatLabMaxCodeTextAssetBytes, true},
		{"ts", "app.ts", "video/mp2t", "code", "ts", drChatLabMaxCodeTextAssetBytes, true}, // browsers report .ts as MPEG-TS!
		{"tsx", "view.tsx", "", "code", "tsx", drChatLabMaxCodeTextAssetBytes, true},
		{"py", "train.py", "text/x-python", "code", "py", drChatLabMaxCodeTextAssetBytes, true},
		{"sql", "schema.sql", "application/octet-stream", "code", "sql", drChatLabMaxCodeTextAssetBytes, true},
		{"uppercase name", "MAIN.GO", "", "code", "go", drChatLabMaxCodeTextAssetBytes, true},
		// image — extension + a matching image/* contentType required
		{"png", "scan.png", "image/png", "image", "png", drMaxImageAssetBytes, true},
		{"jpeg", "photo.jpeg", "image/jpeg", "image", "jpeg", drMaxImageAssetBytes, true},
		{"png with wrong type", "scan.png", "application/octet-stream", "", "png", 0, false},
		// audio — mp3/wav with audio/* type
		{"mp3", "memo.mp3", "audio/mpeg", "audio", "mp3", drChatLabMaxAudioAssetBytes, true},
		{"wav", "memo.wav", "audio/wav", "audio", "wav", drChatLabMaxAudioAssetBytes, true},
		{"mp3 with wrong type", "memo.mp3", "text/plain", "", "mp3", 0, false},
		// pdf — exact type required
		{"pdf", "spec.pdf", "application/pdf", "pdf", "pdf", drChatLabMaxPDFBytes, true},
		{"pdf with wrong type", "spec.pdf", "application/octet-stream", "", "pdf", 0, false},
		// rejected: video, archives, arbitrary binaries
		{"mp4 video", "clip.mp4", "video/mp4", "", "mp4", 0, false},
		{"mov video", "clip.mov", "video/quicktime", "", "mov", 0, false},
		{"zip", "bundle.zip", "application/zip", "", "zip", 0, false},
		{"tar.gz", "bundle.tar.gz", "application/gzip", "", "gz", 0, false},
		{"exe", "tool.exe", "application/octet-stream", "", "exe", 0, false},
		{"no extension", "README", "text/plain", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, ext, maxBytes, ok := projectAssetKind(tc.fileName, tc.contentType)
			if kind != tc.wantKind || ext != tc.wantExt || maxBytes != tc.wantMax || ok != tc.wantOK {
				t.Fatalf("projectAssetKind(%q, %q) = (%q, %q, %d, %v), want (%q, %q, %d, %v)",
					tc.fileName, tc.contentType, kind, ext, maxBytes, ok, tc.wantKind, tc.wantExt, tc.wantMax, tc.wantOK)
			}
		})
	}
}

func TestNormalizeProjectAssetContentType(t *testing.T) {
	cases := []struct {
		kind, in, want string
	}{
		{"code", "text/x-go", drChatLabProjectAssetsCTDefault},
		{"code", "application/octet-stream", drChatLabProjectAssetsCTDefault},
		{"code", "", drChatLabProjectAssetsCTDefault},
		{"code", "video/mp2t", drChatLabProjectAssetsCTDefault},
		{"text", "text/markdown", "text/markdown"},
		{"text", "application/json", "application/json"},
		{"image", "image/png", "image/png"},
		{"pdf", "application/pdf", "application/pdf"},
	}
	for _, tc := range cases {
		if got := normalizeProjectAssetContentType(tc.kind, tc.in); got != tc.want {
			t.Fatalf("normalizeProjectAssetContentType(%q, %q) = %q, want %q", tc.kind, tc.in, got, tc.want)
		}
	}
}

// ---- buildProjectSystemPrompt ----------------------------------------------------

func promptAsset(id, kind, name, ct, key string, size int64) storedProjectAsset {
	return storedProjectAsset{ID: id, Kind: kind, FileName: name, ContentType: ct, S3Key: key, SizeBytes: size}
}

func TestBuildProjectSystemPromptAllSections(t *testing.T) {
	project := projectPromptInput{
		Name:         "OCR Pipeline",
		Description:  "Comparing OCR models.",
		Instructions: "Always answer tersely.",
		Memory:       "- We prefer Claude for handwriting.",
	}
	assets := []storedProjectAsset{
		promptAsset("aaaa-1", "code", "main.go", "text/plain; charset=utf-8", "k1", 14540),
	}
	got, err := buildProjectSystemPrompt(project, assets, true, fakeFetch(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		`You are assisting inside the project "OCR Pipeline".`,
		"## Project description\nComparing OCR models.",
		"## Project instructions — follow these in every response\nAlways answer tersely.",
		"## Project memory\nAccumulated context distilled from earlier chats in this project. Trust it as background, but the current conversation takes precedence if they conflict.\n- We prefer Claude for handwriting.",
		"Use the read_asset tool with an asset's id",
		"- main.go — code, text/plain; charset=utf-8, 14.2 KB — id: aaaa-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildProjectSystemPromptOmitsEmptySections(t *testing.T) {
	got, err := buildProjectSystemPrompt(projectPromptInput{Name: "Bare"}, nil, true, fakeFetch(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `You are assisting inside the project "Bare".` {
		t.Fatalf("bare project prompt should be the single intro line, got:\n%s", got)
	}
	for _, banned := range []string{"## Project description", "## Project instructions", "## Project memory", "## Project assets"} {
		if strings.Contains(got, banned) {
			t.Fatalf("empty section %q must be omitted", banned)
		}
	}
}

func TestBuildProjectSystemPromptNonToolInlining(t *testing.T) {
	small := strings.Repeat("a", 100)
	big := strings.Repeat("b", drChatLabNonToolInlineBudget) // exactly the whole budget — won't fit after `small`
	objects := map[string][]byte{
		"k-small": []byte(small),
		"k-big":   []byte(big),
		"k-tail":  []byte("tail"),
	}
	assets := []storedProjectAsset{
		promptAsset("a1", "text", "small.md", "text/markdown", "k-small", 100),
		promptAsset("a2", "image", "scan.png", "image/png", "k-img", 5000),
		promptAsset("a3", "code", "big.go", "text/plain; charset=utf-8", "k-big", int64(len(big))),
		promptAsset("a4", "text", "tail.txt", "text/plain", "k-tail", 4),
	}
	got, err := buildProjectSystemPrompt(projectPromptInput{Name: "P"}, assets, false, fakeFetch(objects))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// small.md inlined with the documented shape.
	if !strings.Contains(got, "[Asset: small.md]\n```\n"+small+"\n```") {
		t.Fatalf("small.md should be inlined:\n%.400s", got)
	}
	// big.go hits the budget → inlining STOPS; tail.txt (after it) is also skipped.
	if strings.Contains(got, "[Asset: big.go]") || strings.Contains(got, "[Asset: tail.txt]") {
		t.Fatal("assets past the budget stop must not be inlined")
	}
	if !strings.Contains(got, "not included — too large for this model's asset budget: big.go, tail.txt") {
		t.Fatalf("skipped-files note missing or wrong:\n%s", got)
	}
	// Media listed as tool-only; no tool sentence anywhere.
	if !strings.Contains(got, "- scan.png — image — available only when using a tool-capable model") {
		t.Fatalf("media asset note missing:\n%s", got)
	}
	if strings.Contains(got, "read_asset") {
		t.Fatal("non-tool prompt must not mention the read_asset tool")
	}
}

// ---- toolCallAccumulator -----------------------------------------------------------

func TestToolCallAccumulatorSingleCall(t *testing.T) {
	acc := newToolCallAccumulator()
	// First chunk: id + name; continuations: argument fragments only.
	acc.Add(openrouter.ToolCallDelta{Index: 0, ID: "call_1", Type: "function", Function: openrouter.ToolCallFunctionDelta{Name: "read_asset", Arguments: `{"asset`}})
	acc.Add(openrouter.ToolCallDelta{Index: 0, Function: openrouter.ToolCallFunctionDelta{Arguments: `_id":"ab`}})
	acc.Add(openrouter.ToolCallDelta{Index: 0, Function: openrouter.ToolCallFunctionDelta{Arguments: `c"}`}})
	got := acc.Finalize()
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	want := openrouter.ToolCall{ID: "call_1", Type: "function", Function: openrouter.ToolCallFunction{Name: "read_asset", Arguments: `{"asset_id":"abc"}`}}
	if got[0] != want {
		t.Fatalf("call = %+v, want %+v", got[0], want)
	}
}

func TestToolCallAccumulatorParallelInterleaved(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.Add(openrouter.ToolCallDelta{Index: 0, ID: "call_a", Function: openrouter.ToolCallFunctionDelta{Name: "read_asset", Arguments: `{"asset_id":`}})
	acc.Add(openrouter.ToolCallDelta{Index: 1, ID: "call_b", Function: openrouter.ToolCallFunctionDelta{Name: "read_asset", Arguments: `{"asset_id":"2`}})
	acc.Add(openrouter.ToolCallDelta{Index: 0, Function: openrouter.ToolCallFunctionDelta{Arguments: `"1"}`}})
	acc.Add(openrouter.ToolCallDelta{Index: 1, Function: openrouter.ToolCallFunctionDelta{Arguments: `"}`}})
	got := acc.Finalize()
	if len(got) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(got))
	}
	// Finalize preserves first-seen index order.
	if got[0].ID != "call_a" || got[0].Function.Arguments != `{"asset_id":"1"}` {
		t.Fatalf("call 0 = %+v", got[0])
	}
	if got[1].ID != "call_b" || got[1].Function.Arguments != `{"asset_id":"2"}` {
		t.Fatalf("call 1 = %+v", got[1])
	}
	// Type defaults to "function" when the provider omitted it.
	if got[0].Type != "function" {
		t.Fatalf("type should default to function, got %q", got[0].Type)
	}
}

func TestToolCallAccumulatorMissingArgs(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.Add(openrouter.ToolCallDelta{Index: 0, ID: "call_1", Function: openrouter.ToolCallFunctionDelta{Name: "read_asset"}})
	got := acc.Finalize()
	if len(got) != 1 || got[0].Function.Arguments != "" {
		t.Fatalf("missing-args call should finalize with empty arguments: %+v", got)
	}
	if !acc.hasCalls() {
		t.Fatal("hasCalls should be true")
	}
}

// ---- executeReadAsset ---------------------------------------------------------------

func execTestAssets() []storedProjectAsset {
	return []storedProjectAsset{
		promptAsset("id-text", "text", "spec.md", "text/markdown", "k-text", 10),
		promptAsset("id-code", "code", "main.go", "text/plain; charset=utf-8", "k-code", 10),
		promptAsset("id-img", "image", "scan.png", "image/png", "k-img", 10),
		promptAsset("id-audio", "audio", "memo.mp3", "audio/mpeg", "k-audio", 10),
		promptAsset("id-pdf", "pdf", "report.pdf", "application/pdf", "k-pdf", 10),
	}
}

func fullCapModel() *models.DrChatLabModel {
	return &models.DrChatLabModel{ID: "anthropic/claude-opus-4.8", SupportsImages: true, SupportsAudio: true, SupportsTools: true}
}

func TestExecuteReadAssetText(t *testing.T) {
	objects := map[string][]byte{"k-text": []byte("# The Spec\ncontents")}
	exec := executeReadAsset(`{"asset_id":"id-text"}`, execTestAssets(), fullCapModel(), 49152, fakeFetch(objects))
	if exec.ResultText != "# The Spec\ncontents" {
		t.Fatalf("result = %q", exec.ResultText)
	}
	if exec.MediaPart != nil || exec.IsPDF {
		t.Fatal("text read must not produce a media part")
	}
	want := models.DrChatToolActivity{Name: "read_asset", AssetID: "id-text", AssetName: "spec.md", Status: "ok"}
	if exec.Activity != want {
		t.Fatalf("activity = %+v, want %+v", exec.Activity, want)
	}
}

func TestExecuteReadAssetTruncation(t *testing.T) {
	cap := 48 * 1024
	body := strings.Repeat("x", cap+100)
	objects := map[string][]byte{"k-code": []byte(body)}
	exec := executeReadAsset(`{"asset_id":"id-code"}`, execTestAssets(), fullCapModel(), cap, fakeFetch(objects))
	if !strings.HasSuffix(exec.ResultText, "\n…[truncated at 48 KiB]") {
		t.Fatalf("expected truncation suffix, got tail %q", exec.ResultText[len(exec.ResultText)-40:])
	}
	if len(exec.ResultText) > cap+64 {
		t.Fatalf("truncated content too long: %d", len(exec.ResultText))
	}
}

func TestExecuteReadAssetImage(t *testing.T) {
	img := []byte{0x89, 'P', 'N', 'G'}
	exec := executeReadAsset(`{"asset_id":"id-img"}`, execTestAssets(), fullCapModel(), 49152, fakeFetch(map[string][]byte{"k-img": img}))
	if exec.ResultText != "The file 'scan.png' is attached in the next message." {
		t.Fatalf("result = %q", exec.ResultText)
	}
	if exec.MediaPart == nil || exec.MediaPart.Type != "image_url" || exec.MediaPart.ImageURL == nil {
		t.Fatalf("media part = %+v", exec.MediaPart)
	}
	if want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img); exec.MediaPart.ImageURL.URL != want {
		t.Fatalf("image url = %q, want %q", exec.MediaPart.ImageURL.URL, want)
	}

	// Vision-less model → error-text result, no media.
	noVision := &models.DrChatLabModel{ID: "x", SupportsTools: true}
	exec = executeReadAsset(`{"asset_id":"id-img"}`, execTestAssets(), noVision, 49152, fakeFetch(map[string][]byte{"k-img": img}))
	if exec.MediaPart != nil || exec.Activity.Status != "error" || !strings.HasPrefix(exec.ResultText, "Error:") {
		t.Fatalf("vision-less image read should error: %+v", exec)
	}
}

func TestExecuteReadAssetAudio(t *testing.T) {
	audio := []byte{1, 2, 3}
	exec := executeReadAsset(`{"asset_id":"id-audio"}`, execTestAssets(), fullCapModel(), 49152, fakeFetch(map[string][]byte{"k-audio": audio}))
	if exec.MediaPart == nil || exec.MediaPart.Type != "input_audio" || exec.MediaPart.InputAudio == nil {
		t.Fatalf("media part = %+v", exec.MediaPart)
	}
	// Raw base64 (NOT a data URL) + format per the audio docs.
	if exec.MediaPart.InputAudio.Data != base64.StdEncoding.EncodeToString(audio) {
		t.Fatalf("audio data = %q", exec.MediaPart.InputAudio.Data)
	}
	if exec.MediaPart.InputAudio.Format != "mp3" {
		t.Fatalf("audio format = %q, want mp3", exec.MediaPart.InputAudio.Format)
	}

	noAudio := &models.DrChatLabModel{ID: "x", SupportsImages: true, SupportsTools: true}
	exec = executeReadAsset(`{"asset_id":"id-audio"}`, execTestAssets(), noAudio, 49152, fakeFetch(map[string][]byte{"k-audio": audio}))
	if exec.MediaPart != nil || exec.Activity.Status != "error" {
		t.Fatalf("audio on a non-audio model should error: %+v", exec)
	}
}

func TestExecuteReadAssetPDF(t *testing.T) {
	pdf := []byte("%PDF-1.7")
	exec := executeReadAsset(`{"asset_id":"id-pdf"}`, execTestAssets(), fullCapModel(), 49152, fakeFetch(map[string][]byte{"k-pdf": pdf}))
	if !exec.IsPDF {
		t.Fatal("IsPDF should be set")
	}
	if exec.MediaPart == nil || exec.MediaPart.Type != "file" || exec.MediaPart.File == nil || exec.MediaPart.File.Filename != "report.pdf" {
		t.Fatalf("media part = %+v", exec.MediaPart)
	}
}

func TestExecuteReadAssetWrongID(t *testing.T) {
	// An id from ANOTHER project simply doesn't exist in this slice — the
	// executor can never leak other projects' data.
	exec := executeReadAsset(`{"asset_id":"other-project-asset"}`, execTestAssets(), fullCapModel(), 49152, fakeFetch(nil))
	if exec.Activity.Status != "error" || exec.Activity.AssetName != "unknown" {
		t.Fatalf("activity = %+v", exec.Activity)
	}
	if !strings.Contains(exec.ResultText, "no asset with id") {
		t.Fatalf("result = %q", exec.ResultText)
	}
}

func TestExecuteReadAssetBadArgs(t *testing.T) {
	for _, args := range []string{"", "{", `{"asset_id":""}`, `{"wrong":"key"}`} {
		exec := executeReadAsset(args, execTestAssets(), fullCapModel(), 49152, fakeFetch(nil))
		if exec.Activity.Status != "error" || !strings.HasPrefix(exec.ResultText, "Error:") {
			t.Fatalf("args %q should produce an error result, got %+v", args, exec)
		}
	}
}

func TestSyntheticMediaMessage(t *testing.T) {
	part := openrouter.ContentPart{Type: "image_url", ImageURL: &openrouter.ImageURL{URL: "data:image/png;base64,AA=="}}
	msg := syntheticMediaMessage([]openrouter.ContentPart{part})
	if msg.Role != "user" {
		t.Fatalf("role = %q", msg.Role)
	}
	parts, ok := msg.Content.([]openrouter.ContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %+v", msg.Content)
	}
	if parts[0].Type != "text" || parts[0].Text != "Attached asset(s) from read_asset:" {
		t.Fatalf("text part = %+v", parts[0])
	}
}

// ---- usage summation across rounds ------------------------------------------------

func TestUsageTotalsAcrossRounds(t *testing.T) {
	totals := usageTotals{}
	if totals.toUsage() != nil {
		t.Fatal("no usage seen → nil")
	}
	totals.add(nil) // nil-safe
	totals.add(&openrouter.Usage{PromptTokens: 100, CompletionTokens: 20, Cost: 0.001,
		CompletionTokensDetails: &openrouter.CompletionTokensDetails{ReasoningTokens: 5}})
	totals.add(&openrouter.Usage{PromptTokens: 250, CompletionTokens: 80, Cost: 0.004})
	u := totals.toUsage()
	if u == nil {
		t.Fatal("expected summed usage")
	}
	if u.PromptTokens != 350 || u.CompletionTokens != 100 || u.ReasoningTokens() != 5 {
		t.Fatalf("summed usage = %+v", u)
	}
	if diff := u.Cost - 0.005; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("summed cost = %v", u.Cost)
	}
}

// ---- singleflightLatest --------------------------------------------------------------

func TestSingleflightLatestCoalesces(t *testing.T) {
	var s singleflightLatest
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	runs := 0

	run := func() {
		mu.Lock()
		runs++
		n := runs
		mu.Unlock()
		if n == 1 {
			close(started)
			<-release // hold the first run open while triggers pile up
		}
	}

	done := make(chan struct{})
	go func() {
		s.Do("p1", run)
		close(done)
	}()
	<-done // Do returns immediately (run executes on its own goroutine)
	<-started

	// Three triggers while running → exactly ONE trailing rerun.
	s.Do("p1", run)
	s.Do("p1", run)
	s.Do("p1", run)
	close(release)

	// Wait for the flight to fully drain: a subsequent Do must start a FRESH
	// run (proving the map entry was cleared).
	drained := make(chan struct{})
	for {
		s.mu.Lock()
		_, inFlight := s.flights["p1"]
		s.mu.Unlock()
		if !inFlight {
			close(drained)
			break
		}
	}
	<-drained

	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 2 {
		t.Fatalf("expected exactly 2 runs (initial + one coalesced rerun), got %d", got)
	}

	// Fresh trigger after drain → a third run.
	finished := make(chan struct{})
	s.Do("p1", func() { close(finished) })
	<-finished
}

func TestSingleflightLatestIndependentKeys(t *testing.T) {
	var s singleflightLatest
	var wg sync.WaitGroup
	wg.Add(2)
	s.Do("a", wg.Done)
	s.Do("b", wg.Done)
	wg.Wait() // both keys run — no cross-key blocking
}

// ---- memory prompt builder --------------------------------------------------------------

func TestBuildMemoryPrompt(t *testing.T) {
	long := strings.Repeat("m", drChatLabMemoryMsgTruncate+50)
	system, user := buildMemoryPrompt(memoryPromptInput{
		Name:          "OCR Pipeline",
		Description:   "Comparing OCR models.",
		Instructions:  "Terse answers.",
		CurrentMemory: "Old memory.",
		Assets:        []storedProjectAsset{promptAsset("a1", "code", "main.go", "text/plain", "k", 2048)},
		Messages: []memoryTranscriptEntry{
			{SessionTitle: "Kickoff", Role: "user", Content: "Which model won?"},
			{SessionTitle: "Kickoff", Role: "assistant", Content: long},
		},
		MaxChars: 4096,
	})

	// The system prompt is exactly the six-section instruction (the feedback-
	// distillation guidance now lives inside the Key learnings charter).
	if system != fmt.Sprintf(drChatLabMemoryInstruction, 4096) {
		t.Fatalf("system prompt = %q", system)
	}
	if !strings.Contains(system, "Maximum 4096 characters") {
		t.Fatalf("char cap not interpolated: %q", system)
	}
	for _, want := range []string{
		"# Project: OCR Pipeline",
		"## Description\nComparing OCR models.",
		"## Instructions\nTerse answers.",
		"## Current memory (rewrite this from scratch — do not append)\nOld memory.",
		"- main.go (code, 2.0 KB)",
		"[Kickoff] user: Which model won?",
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("memory prompt missing %q:\n%.600s", want, user)
		}
	}
	// The long assistant message is truncated to 2 KiB (+ ellipsis).
	if strings.Contains(user, long) {
		t.Fatal("long message should be truncated")
	}
	if !strings.Contains(user, "[Kickoff] assistant: "+strings.Repeat("m", drChatLabMemoryMsgTruncate)+"…") {
		t.Fatal("truncated message with prefix + ellipsis missing")
	}
}

func TestBuildMemoryPromptOmitsEmptySections(t *testing.T) {
	_, user := buildMemoryPrompt(memoryPromptInput{Name: "Bare", MaxChars: 1000})
	for _, banned := range []string{"## Description", "## Instructions", "## Current memory", "## Assets on file", "## Recent messages"} {
		if strings.Contains(user, banned) {
			t.Fatalf("empty section %q must be omitted:\n%s", banned, user)
		}
	}
}

func TestSanitizeProjectMemory(t *testing.T) {
	if got := sanitizeProjectMemory("  hi  ", 4096); got != "hi" {
		t.Fatalf("trim failed: %q", got)
	}
	// Under the cap: markdown headings pass through untouched.
	structured := "## Purpose & context\n- A lab.\n\n## Current state\n- Built."
	if got := sanitizeProjectMemory(structured, 4096); got != structured {
		t.Fatalf("markdown must be preserved: %q", got)
	}
	// Over the cap: truncation lands on a LINE boundary (never mid-line) with
	// an appended ellipsis, and stays within the cap.
	long := "## Purpose & context\n" + strings.Repeat("- bullet line\n", 400)
	got := sanitizeProjectMemory(long, 512)
	if n := len([]rune(got)); n > 512 {
		t.Fatalf("cap exceeded: %d runes", n)
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Fatalf("truncated memory must end with an ellipsis line: %q", got[len(got)-20:])
	}
	if !strings.Contains(got, "## Purpose & context") {
		t.Fatal("heading stripped by truncation")
	}
	body := strings.TrimSuffix(got, "\n…")
	for _, line := range strings.Split(body, "\n")[1:] {
		if line != "- bullet line" {
			t.Fatalf("line sliced mid-way: %q", line)
		}
	}
	// A single line with no newline still truncates with the ellipsis.
	oneLine := strings.Repeat("y", 100)
	got = sanitizeProjectMemory(oneLine, 50)
	if n := len([]rune(got)); n > 50 {
		t.Fatalf("single-line cap exceeded: %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("single-line truncation must end with …: %q", got)
	}
}

// ---- Assistant tool-call continuation message (reasoning preserved verbatim) --------------

func TestAssistantToolCallMessageShape(t *testing.T) {
	details := []json.RawMessage{json.RawMessage(`{"type":"reasoning.text","text":"check the file","signature":"sig1"}`)}
	msg := openrouter.Message{
		Role:             "assistant",
		Content:          "Let me look.",
		ToolCalls:        []openrouter.ToolCall{{ID: "call_1", Type: "function", Function: openrouter.ToolCallFunction{Name: "read_asset", Arguments: `{"asset_id":"a1"}`}}},
		ReasoningDetails: details,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":"Let me look.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_asset","arguments":"{\"asset_id\":\"a1\"}"}}],"reasoning_details":[{"type":"reasoning.text","text":"check the file","signature":"sig1"}]}`
	if string(raw) != want {
		t.Fatalf("assistant tool-call message =\n%s\nwant\n%s", raw, want)
	}

	// Tool result message shape.
	result := openrouter.Message{Role: "tool", ToolCallID: "call_1", Content: "file contents"}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"role":"tool","content":"file contents","tool_call_id":"call_1"}` {
		t.Fatalf("tool result message = %s", raw)
	}
}

// ---- Tool SSE event wire shape -----------------------------------------------------------

func TestChatLabToolEventMarshaling(t *testing.T) {
	raw, err := json.Marshal(chatLabToolEvent{Type: "tool", Name: "read_asset", AssetID: "a1", AssetName: "spec.md", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"tool","name":"read_asset","assetId":"a1","assetName":"spec.md","status":"running"}`; string(raw) != want {
		t.Fatalf("tool event = %s, want %s", raw, want)
	}
}

// ---- chatLabHumanSize ----------------------------------------------------------------------

func TestChatLabHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{14540, "14.2 KB"},
		{200 * 1024, "200 KB"},
		{3 * 1024 * 1024, "3.0 MB"},
		{25 * 1024 * 1024, "25 MB"},
	}
	for _, tc := range cases {
		if got := chatLabHumanSize(tc.in); got != tc.want {
			t.Fatalf("chatLabHumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Guard: catalog capability flags for tools/audio come through buildChatLabModel.
func TestBuildChatLabModelToolsAudioFlags(t *testing.T) {
	m := buildChatLabModel(openrouter.Model{
		ID:                  "anthropic/claude-opus-4.8",
		Architecture:        openrouter.Architecture{InputModalities: []string{"text", "image", "audio"}},
		SupportedParameters: []string{"tools", "reasoning"},
	})
	if !m.SupportsTools || !m.SupportsAudio {
		t.Fatalf("flags = %+v", m)
	}
	plain := buildChatLabModel(openrouter.Model{ID: "x/y"})
	if plain.SupportsTools || plain.SupportsAudio {
		t.Fatalf("plain model flags = %+v", plain)
	}
}

// Sanity: an unexpected fetch error propagates as an error-text result, never a panic.
func TestExecuteReadAssetFetchError(t *testing.T) {
	exec := executeReadAsset(`{"asset_id":"id-text"}`, execTestAssets(), fullCapModel(), 49152,
		func(string) ([]byte, error) { return nil, errors.New("s3 down") })
	if exec.Activity.Status != "error" || !strings.HasPrefix(exec.ResultText, "Error:") {
		t.Fatalf("fetch failure should be an error result: %+v", exec)
	}
}
