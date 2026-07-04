package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func outputStrings(outs []models.DocumentScanOutput) []string {
	s := make([]string, len(outs))
	for i, o := range outs {
		s[i] = string(o)
	}
	return s
}

func eqStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNormalizeDocumentScanOutputs(t *testing.T) {
	t.Run("defaults to pdf when empty", func(t *testing.T) {
		got, err := NormalizeDocumentScanOutputs(nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if !eqStringSlices(outputStrings(got), []string{"pdf"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("dedupes + normalizes case", func(t *testing.T) {
		got, err := NormalizeDocumentScanOutputs([]string{"PDF", "pdf", "Docx"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !eqStringSlices(outputStrings(got), []string{"pdf", "docx"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("rejects docx when disabled", func(t *testing.T) {
		if _, err := NormalizeDocumentScanOutputs([]string{"docx"}, false); err == nil {
			t.Fatal("expected error when docx disabled")
		}
	})
	t.Run("rejects unknown output", func(t *testing.T) {
		if _, err := NormalizeDocumentScanOutputs([]string{"xlsx"}, true); err == nil {
			t.Fatal("expected rejection of unknown output")
		}
	})
}

func TestNormalizeDocumentScanMode(t *testing.T) {
	cases := map[string]struct {
		want models.DocumentScanContentMode
		err  bool
	}{
		"":            {models.DocumentScanModeAuto, false},
		"auto":        {models.DocumentScanModeAuto, false},
		"PRINTED":     {models.DocumentScanModePrinted, false},
		"handwriting": {models.DocumentScanModeHandwriting, false},
		"nonsense":    {"", true},
	}
	for in, c := range cases {
		got, err := NormalizeDocumentScanMode(in)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error", in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %q err %v", in, got, err)
		}
	}
}

func TestNormalizeDocumentScanLanguage(t *testing.T) {
	allow := []string{"eng", "fra", "spa"}
	t.Run("defaults to first allowlisted", func(t *testing.T) {
		got, err := NormalizeDocumentScanLanguage("", allow)
		if err != nil || got != "eng" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("accepts multi-language", func(t *testing.T) {
		got, err := NormalizeDocumentScanLanguage("eng+fra", allow)
		if err != nil || got != "eng+fra" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("rejects not-allowlisted", func(t *testing.T) {
		if _, err := NormalizeDocumentScanLanguage("deu", allow); err == nil {
			t.Fatal("expected rejection of non-allowlisted code")
		}
	})
	t.Run("rejects shell injection", func(t *testing.T) {
		for _, bad := range []string{"eng; rm -rf /", "eng`whoami`", "eng/../x", "eng eng"} {
			if _, err := NormalizeDocumentScanLanguage(bad, allow); err == nil {
				t.Fatalf("expected rejection of %q", bad)
			}
		}
	})
}

func TestOrderDocumentScanImages(t *testing.T) {
	fields := []string{"image_0", "image_1", "image_2"}
	t.Run("explicit order respected", func(t *testing.T) {
		got, err := OrderDocumentScanImages(fields, []string{"image_2", "image_0", "image_1"})
		if err != nil {
			t.Fatal(err)
		}
		if !eqStringSlices(got, []string{"image_2", "image_0", "image_1"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("empty order sorts field names", func(t *testing.T) {
		got, err := OrderDocumentScanImages([]string{"image_2", "image_0", "image_1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !eqStringSlices(got, []string{"image_0", "image_1", "image_2"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("rejects unknown field", func(t *testing.T) {
		if _, err := OrderDocumentScanImages(fields, []string{"image_0", "image_9", "image_1"}); err == nil {
			t.Fatal("expected rejection of unknown field name")
		}
	})
	t.Run("rejects partial order", func(t *testing.T) {
		if _, err := OrderDocumentScanImages(fields, []string{"image_0"}); err == nil {
			t.Fatal("expected rejection of incomplete order")
		}
	})
	t.Run("rejects duplicate order", func(t *testing.T) {
		if _, err := OrderDocumentScanImages(fields, []string{"image_0", "image_0", "image_1"}); err == nil {
			t.Fatal("expected rejection of duplicate order entry")
		}
	})
}

func TestValidateDocumentScanCounts(t *testing.T) {
	cfg := &config.Config{DocumentScanMaxImages: 5, DocumentScanMaxPages: 5}
	if err := ValidateDocumentScanCounts(0, cfg); err == nil {
		t.Fatal("expected error for zero images")
	}
	if err := ValidateDocumentScanCounts(3, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateDocumentScanCounts(6, cfg); err == nil {
		t.Fatal("expected error for too many images")
	}
}

func TestNormalizeDocumentScanEngines(t *testing.T) {
	if got, err := NormalizeDocumentScanStructureEngine("", "paddleocr-vl"); err != nil || got != "paddleocr-vl" {
		t.Fatalf("structure default: got %q err %v", got, err)
	}
	if _, err := NormalizeDocumentScanStructureEngine("tesseract", "paddleocr-vl"); err == nil {
		t.Fatal("expected rejection of invalid structure engine")
	}
	if got, err := NormalizeDocumentScanSecondOpinionEngine("none", "paddleocr-vl"); err != nil || got != "none" {
		t.Fatalf("second opinion none: got %q err %v", got, err)
	}
	if _, err := NormalizeDocumentScanSecondOpinionEngine("bogus", "paddleocr-vl"); err == nil {
		t.Fatal("expected rejection of invalid second-opinion engine")
	}
}

func stageKeySet(stages []models.TranscodeJobStage) map[string]bool {
	set := map[string]bool{}
	for _, s := range stages {
		set[s.Key] = true
	}
	return set
}

func TestDocumentScanBuildStages(t *testing.T) {
	svc := NewDocumentScanService(&config.Config{}, nil, nil, nil, nil)

	t.Run("printed pdf only", func(t *testing.T) {
		req := DocumentScanRequest{
			Mode:    models.DocumentScanModePrinted,
			Outputs: []models.DocumentScanOutput{models.DocumentScanOutputPDF},
		}
		keys := stageKeySet(svc.BuildStages(req))
		for _, want := range []string{"queued", "prepare", "recognize", "build_pdf", "package", "completed"} {
			if !keys[want] {
				t.Errorf("missing stage %q", want)
			}
		}
		for _, absent := range []string{"classify", "verify", "second_opinion", "build_docx", "summarize"} {
			if keys[absent] {
				t.Errorf("unexpected stage %q for printed-pdf", absent)
			}
		}
	})

	t.Run("auto full pipeline", func(t *testing.T) {
		req := DocumentScanRequest{
			Mode:                models.DocumentScanModeAuto,
			Outputs:             []models.DocumentScanOutput{models.DocumentScanOutputPDF, models.DocumentScanOutputDOCX},
			Verify:              true,
			SecondOpinion:       true,
			SecondOpinionEngine: "paddleocr-vl",
			Summarize:           true,
		}
		keys := stageKeySet(svc.BuildStages(req))
		for _, want := range []string{"classify", "recognize", "verify", "second_opinion", "build_pdf", "build_docx", "summarize"} {
			if !keys[want] {
				t.Errorf("missing stage %q in full auto pipeline", want)
			}
		}
	})

	t.Run("handwriting without docx/summary", func(t *testing.T) {
		req := DocumentScanRequest{
			Mode:    models.DocumentScanModeHandwriting,
			Outputs: []models.DocumentScanOutput{models.DocumentScanOutputPDF},
			Verify:  true,
		}
		keys := stageKeySet(svc.BuildStages(req))
		if keys["classify"] {
			t.Error("classify should be absent when mode != auto")
		}
		if !keys["verify"] {
			t.Error("verify should be present")
		}
		if keys["second_opinion"] {
			t.Error("second_opinion should be absent when not requested")
		}
		if keys["build_docx"] {
			t.Error("build_docx should be absent when docx not requested")
		}
	})

	t.Run("second_opinion none suppresses stage", func(t *testing.T) {
		req := DocumentScanRequest{
			Mode:                models.DocumentScanModeHandwriting,
			Outputs:             []models.DocumentScanOutput{models.DocumentScanOutputPDF},
			SecondOpinion:       true,
			SecondOpinionEngine: "none",
		}
		if stageKeySet(svc.BuildStages(req))["second_opinion"] {
			t.Error("second_opinion stage should be suppressed when engine is none")
		}
	})

	t.Run("progress is monotonic and ends at 100", func(t *testing.T) {
		req := DocumentScanRequest{
			Mode:    models.DocumentScanModeAuto,
			Outputs: []models.DocumentScanOutput{models.DocumentScanOutputPDF, models.DocumentScanOutputDOCX},
			Verify:  true,
		}
		stages := svc.BuildStages(req)
		last := -1
		for _, s := range stages {
			if s.Progress < last {
				t.Fatalf("non-monotonic progress at %q: %d < %d", s.Key, s.Progress, last)
			}
			last = s.Progress
		}
		if stages[len(stages)-1].Progress != 100 {
			t.Fatalf("final stage progress = %d, want 100", stages[len(stages)-1].Progress)
		}
	})
}

func TestIsDocumentScanLangCode(t *testing.T) {
	for _, ok := range []string{"eng", "fra", "chi_sim", "deu"} {
		if !isDocumentScanLangCode(ok) {
			t.Errorf("expected %q valid", ok)
		}
	}
	for _, bad := range []string{"e", "eng1", "eng;", strings.Repeat("a", 20), "EN"} {
		if isDocumentScanLangCode(bad) {
			t.Errorf("expected %q invalid", bad)
		}
	}
}
