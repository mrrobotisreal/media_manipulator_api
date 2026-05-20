package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultCaptionsModel is the Ollama model name we expect to be available on
// the GPU host. It corresponds to the custom Modelfile at
// internal/ai/Modelfile.mm-captions-translategemma-12b — a translategemma:12b
// derivative tuned for subtitle translation with strong structure preservation
// (temperature 0, top_p 0.9, repeat_penalty 1.03, num_ctx 32768) and a SYSTEM
// prompt that expects the structured user-prompt header format we build in
// buildTranslationPrompt below.
//
// Override with OLLAMA_CAPTIONS_MODEL if you ever need to A/B against another
// translation model without redeploying the binary.
const defaultCaptionsModel = "mm-captions-translategemma-12b"

// captionsTranslationModel returns the resolved Ollama model name.
func captionsTranslationModel() string {
	return envOrDefault("OLLAMA_CAPTIONS_MODEL", defaultCaptionsModel)
}

// TranslateCaptionsSegment is one cue we feed into and read back from the
// translator. We deliberately keep `id` so the model can preserve order, and
// we always restore the original `start`/`end` after parsing to defend against
// any edge case where the model edits timings despite its SYSTEM-prompt
// instructions.
type TranslateCaptionsSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// translateSegmentsBatch hands the model a `{"segments":[...]}` JSON payload
// and asks for the same shape back with `text` translated. Batched by
// batchSize so a single call never approaches the model's num_ctx ceiling on
// very long videos.
//
// targetLang is a BCP-47 code (e.g. "es", "pt-BR", "zh-Hans"). The function
// pins original timings back on the way out so the downstream VTT writer
// always uses whisper's timing source-of-truth.
func translateSegmentsBatch(ctx context.Context, segments []TranslateCaptionsSegment, sourceLang, targetLang string, batchSize int) ([]TranslateCaptionsSegment, error) {
	if batchSize <= 0 {
		batchSize = 30
	}
	out := make([]TranslateCaptionsSegment, 0, len(segments))
	for start := 0; start < len(segments); start += batchSize {
		end := start + batchSize
		if end > len(segments) {
			end = len(segments)
		}
		batch := segments[start:end]
		translated, err := callOllamaTranslate(ctx, batch, sourceLang, targetLang)
		if err != nil {
			return nil, fmt.Errorf("translate batch %d-%d: %w", start, end, err)
		}
		// Pin timings (and the cue ID) back to the originals — the SYSTEM
		// prompt forbids mutating them but we don't trust untrusted output.
		// We match by *index*, not by id, because the model has been observed
		// (rarely, in stress tests) to rewrite ids when reordering its
		// internal scratchpad even though the spec forbids it. Index-pinning
		// is the strongest invariant we can keep here.
		for i, seg := range translated {
			if i >= len(batch) {
				break
			}
			seg.ID = batch[i].ID
			seg.Start = batch[i].Start
			seg.End = batch[i].End
			out = append(out, seg)
		}
	}
	return out, nil
}

// callOllamaTranslate makes a single non-streaming chat completion. Format is
// forced to JSON at the Ollama API level so we can decode the body directly;
// the user-prompt header also declares `Format: json` so the model knows it
// should return the JSON variant of its supported output formats (rule 31 in
// the Modelfile SYSTEM prompt).
//
// We do NOT pass `options.temperature` here: the Modelfile already sets it to
// 0 and overriding it would mean the deployed model semantics change behind
// the operator's back if they tune the Modelfile.
func callOllamaTranslate(ctx context.Context, segments []TranslateCaptionsSegment, sourceLang, targetLang string) ([]TranslateCaptionsSegment, error) {
	prompt := buildTranslationPrompt(segments, sourceLang, targetLang)
	payload := map[string]any{
		"model":    captionsTranslationModel(),
		"stream":   false,
		"format":   "json",
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(envOrDefault("OLLAMA_URL", "http://localhost:11434"), "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Duration(envInt("OLLAMA_TIMEOUT_SECONDS", 300)) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var msg map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		return nil, fmt.Errorf("ollama status %d: %v", resp.StatusCode, msg)
	}
	var envelope struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	content := strings.TrimSpace(envelope.Message.Content)
	if content == "" {
		return nil, fmt.Errorf("empty translation response from %s", captionsTranslationModel())
	}
	// We always send the wrapped `{"segments":[...]}` shape so the model
	// returns the same wrapper. Tolerate a bare array as well in case a
	// future model tweak drops the wrapper.
	var wrapper struct {
		Segments []TranslateCaptionsSegment `json:"segments"`
	}
	if err := json.Unmarshal([]byte(content), &wrapper); err == nil && len(wrapper.Segments) > 0 {
		return wrapper.Segments, nil
	}
	var bare []TranslateCaptionsSegment
	if err := json.Unmarshal([]byte(content), &bare); err == nil {
		return bare, nil
	}
	return nil, fmt.Errorf("could not parse translation response: %s", tail(content, 500))
}

// buildTranslationPrompt assembles the exact user-prompt header format that
// mm-captions-translategemma-12b expects (see internal/ai/Modelfile.mm-captions-translategemma-12b
// SYSTEM prompt, "Expected user prompt format" section):
//
//	Target language: <BCP-47 code>
//	Target language name: <display name>
//	Source language: <optional source language or auto>
//	Format: <webvtt|srt|json|plain>
//	Instructions: <optional glossary/style notes>
//
//	Caption content:
//	<caption file or segment payload>
//
// We pick Format: json because it matches our segment shape end-to-end and the
// model's rule 31 guarantees schema preservation.
func buildTranslationPrompt(segments []TranslateCaptionsSegment, sourceLang, targetLang string) string {
	if strings.TrimSpace(sourceLang) == "" {
		sourceLang = "auto"
	}
	targetDisplay := displayNameForCode(targetLang)
	if targetDisplay == "" {
		targetDisplay = strings.ToUpper(targetLang)
	}
	wrapper := struct {
		Segments []TranslateCaptionsSegment `json:"segments"`
	}{Segments: segments}
	body, _ := json.Marshal(wrapper)

	var b strings.Builder
	fmt.Fprintf(&b, "Target language: %s\n", targetLang)
	fmt.Fprintf(&b, "Target language name: %s\n", targetDisplay)
	fmt.Fprintf(&b, "Source language: %s\n", sourceLang)
	b.WriteString("Format: json\n")
	b.WriteString(`Instructions: The caption content is a JSON object with a "segments" array. Each segment object has the fields id, start, end, and text. Preserve id, start, and end exactly. Translate only the text field, one segment at a time, keeping each translated cue concise and subtitle-friendly. Do not add, remove, merge, or split segments. Return the same JSON schema: {"segments":[{"id":number,"start":number,"end":number,"text":string}]} with no surrounding prose, code fences, or explanation.`)
	b.WriteString("\n\n")
	b.WriteString("Caption content:\n")
	b.Write(body)
	return b.String()
}

// OllamaReachable does a quick GET /api/tags so we can fail fast when
// translation is requested but the backend isn't up. We don't verify the
// specific translation model is loaded — if it's missing, the chat call will
// error with a clear "model not found" message that the pipeline records as
// a warning per-language without aborting the job.
func OllamaReachable(ctx context.Context) bool {
	url := strings.TrimRight(envOrDefault("OLLAMA_URL", "http://localhost:11434"), "/") + "/api/tags"
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}
