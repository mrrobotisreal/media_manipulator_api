package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Golden .ass parity (MM121 decision 4). Every testdata/captions/<name>.json
// is one (cues, style, canvas) input; <name>.ass is the exact text BuildASS
// must produce. The UI's TS generator (lib/studio/captions/ass.ts) reads the
// SAME files (copied to lib/studio/captions/goldens/ with an identity test)
// and must produce the same bytes. Regenerate after an intentional generator
// change with:
//
//	UPDATE_CAPTION_GOLDENS=1 go test ./internal/services -run TestCaptionASSGoldens
//
// then copy testdata/captions/ to the UI (see lib/studio/captions/goldens/README).

type captionGoldenFixture struct {
	Name   string                     `json:"name"`
	Width  int                        `json:"width"`
	Height int                        `json:"height"`
	Style  *models.StudioCaptionStyle `json:"style"`
	Cues   []models.StudioCaptionCue  `json:"cues"`
}

func captionGoldenFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "captions", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestCaptionASSGoldens(t *testing.T) {
	paths := captionGoldenFixtures(t)
	if len(paths) < 30 {
		t.Fatalf("want ≥ 30 caption golden fixtures (MM121 18 + MM122 12), have %d", len(paths))
	}
	update := os.Getenv("UPDATE_CAPTION_GOLDENS") == "1"
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var fx captionGoldenFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if fx.Name+".json" != filepath.Base(p) {
			t.Errorf("%s: name %q does not match the file name", p, fx.Name)
		}
		got := BuildASS(fx.Cues, fx.Style, fx.Width, fx.Height, "")
		assPath := strings.TrimSuffix(p, ".json") + ".ass"
		if update {
			if err := os.WriteFile(assPath, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(assPath)
		if err != nil {
			t.Fatalf("%s: %v (run with UPDATE_CAPTION_GOLDENS=1)", assPath, err)
		}
		if got != string(want) {
			t.Errorf("%s: BuildASS drifted from the golden; regenerate deliberately and copy to the UI\n--- got\n%s", fx.Name, got)
		}
	}
}

// TestCaptionGoldenCoverage pins the fixture matrix decision 4 asks for.
func TestCaptionGoldenCoverage(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range captionGoldenFixtures(t) {
		raw, _ := os.ReadFile(p)
		var fx captionGoldenFixture
		_ = json.Unmarshal(raw, &fx)
		st := models.SanitizeCaptionStyle(fx.Style)
		anim := models.CaptionAnimationNone
		if st != nil && st.Animation != "" {
			anim = st.Animation
		}
		words := false
		for _, c := range fx.Cues {
			words = words || len(c.Words) > 0
		}
		orient := "landscape"
		if fx.Height > fx.Width {
			orient = "vertical"
		}
		w := "nowords"
		if words {
			w = "words"
		}
		seen[anim+"/"+w] = true
		seen[orient] = true
		if st != nil {
			seen["bg:"+st.BackgroundMode] = true
			seen["pos:"+st.Position] = true
			if st.Uppercase != nil && *st.Uppercase {
				seen["uppercase"] = true
			}
		}
		for _, c := range fx.Cues {
			for _, w := range c.Words {
				if w.Emphasis {
					seen["emphasis"] = true
				}
			}
			for _, r := range c.Text {
				if s := models.CaptionScriptOf(r); s != "" {
					seen["script:"+s] = true
				}
			}
		}
	}
	// MM122: every animation, with and without word timings.
	for _, a := range models.CaptionAnimations {
		for _, w := range []string{"words", "nowords"} {
			if !seen[a+"/"+w] {
				t.Errorf("no fixture for %s %s", a, w)
			}
		}
	}
	for _, k := range []string{"landscape", "vertical", "uppercase", "emphasis", "bg:word_box", "bg:box", "bg:none", "pos:middle", "script:hebrew", "script:arabic", "script:han", "script:cyrillic"} {
		if !seen[k] {
			t.Errorf("no fixture covers %s", k)
		}
	}
}
