package burst

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Kind coverage (MM94 slot-card pattern): every Kind the services package can
// pass to a GPURequest has a profile row, so a new GPU tool cannot silently
// become burstable or be forgotten.
func TestProfiles_KindCoverage(t *testing.T) {
	files, _ := filepath.Glob("../services/*.go")
	if len(files) == 0 {
		t.Fatal("no services files found")
	}
	// Dynamic kinds are built from a prefix; the sample stands in for them.
	dynamic := map[string][]string{
		`"ollama[" + o.Caller + "]"`:           {"ollama[caption-translate]"},
		`"voices-" + tool`:                     {"voices-tts"},
		`"restore-" + string(id)`:              {"restore-vrt"},
		`"image-restore-" + string(u.Model)`:   {"image-restore-hat"},
		`aiGPUKeyFacePrivacy + "[detect]"`:     {"face_privacy[detect]"},
		`"document-scan-" + mode`:              {"document-scan-printed"},
		`"ollama[document-scan-" + mode + "]"`: {"ollama[document-scan-printed]"},
	}
	kindRe := regexp.MustCompile(`(?m)\bKind(?:\s*:|\s*=)\s*(.+?)(?:,\s*)?$`)
	constRe := regexp.MustCompile(`(?m)^\s*aiGPUKey\w+\s*=\s*"([^"]+)"`)
	photoRe := regexp.MustCompile(`"((?:rembg|lama)\[photo\])"`)
	found := map[string]bool{}
	var used int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		src := string(b)
		for _, m := range constRe.FindAllStringSubmatch(src, -1) {
			found[m[1]] = true
		}
		for _, m := range photoRe.FindAllStringSubmatch(src, -1) {
			found[m[1]] = true
		}
		// Only GPURequest literals / Kind assignments on a gpuReq.
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "Kind") {
				continue
			}
			m := kindRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			expr := strings.TrimSpace(m[1])
			if lit, ok := dynamic[expr]; ok {
				for _, l := range lit {
					found[l] = true
				}
				used++
				continue
			}
			if strings.HasPrefix(expr, `"`) && strings.HasSuffix(expr, `"`) && strings.Count(expr, `"`) == 2 {
				k := strings.Trim(expr, `"`)
				// Only scheduler kinds: skip JSON/sidecar/media Kind fields.
				if gpuKindLine(src, line) {
					found[k] = true
				}
			}
		}
	}
	if used == 0 {
		t.Fatal("dynamic kind patterns matched nothing: the source drifted, update the test")
	}
	var missing []string
	for k := range found {
		if _, ok := Lookup(k); !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("GPU Kinds with no burst profile row (add them, burstable or Never): %v", missing)
	}
	for _, want := range []string{"remove_background", "whisper-ct2", "whisper-ct2[analysis]", "rembg[photo]", "face_privacy", "demucs"} {
		if !found[want] {
			t.Errorf("coverage scan never saw %q: scanner drifted", want)
		}
	}
}

// gpuKindLine reports whether the Kind on this line belongs to a GPURequest
// literal or a gpuReq assignment (not a document-scan sidecar or MediaKind).
func gpuKindLine(src, line string) bool {
	if strings.Contains(line, "gpuReq.Kind") || strings.Contains(line, "gpuReq") {
		return true
	}
	i := strings.Index(src, line)
	if i < 0 {
		return false
	}
	start := i - 120
	if start < 0 {
		start = 0
	}
	return strings.Contains(src[start:i], "GPURequest{")
}

func TestProfiles_NeverRowsAreExplicit(t *testing.T) {
	for _, k := range []string{"face_privacy", "face_privacy[detect]", "rembg[photo]", "lama[photo]", "whisper-ct2[analysis]"} {
		p, ok := Lookup(k)
		if !ok || !p.Never || p.NeverReason == "" {
			t.Errorf("%s must be an explicit Never row with a reason: %+v", k, p)
		}
		if Burstable(k) {
			t.Errorf("%s must not be burstable", k)
		}
	}
	if !Burstable("whisper-ct2") || !Burstable("remove_background") || !Burstable("restore-vrt") || !Burstable("ollama[x]") || !Burstable("voices-tts") {
		t.Error("expected burstable kinds")
	}
	if Burstable("brand-new-tool") {
		t.Error("unknown kind must not be burstable")
	}
}

func TestProfiles_CandidateOverride(t *testing.T) {
	p, _ := Lookup("demucs")
	if got := CandidatesFor(p, nil); got[0] != "g6.xlarge" {
		t.Fatal(got)
	}
	if got := CandidatesFor(p, map[string][]string{"AUDIO": {"g6e.xlarge"}}); got[0] != "g6e.xlarge" {
		t.Fatal(got)
	}
}
