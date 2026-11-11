package burst

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// On 2026-11-11 the first lab-voice build died at its first model: the
// installer passed `--arg torch` to jq and read `$torchv`. The dry-run tests
// print the plan and never run a step, so nothing had ever executed that line.
// This test runs the WHOLE lab-voice install sequence, every shell line of it,
// against stand-ins for the tools that download (uv, hf, git, apt, aws), so a
// typo in any step fails here and not on a rented GPU.
func TestLabVoiceInstallSequenceRunsEndToEndAgainstStandIns(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	realPy, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	fakes := t.TempDir()
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The python inside a model venv: answers the torch version, runs anything else as a no-op.
	write(fakes, "venvpython", `#!/bin/bash
echo "venvpython $*" >> "$MM_REC/venvpython.log"
case "$*" in *"import torch"*) echo "2.8.0+cu128" ;; esac
exit 0
`)
	write(fakes, "pip", "#!/bin/bash\necho \"pip $*\" >> \"$MM_REC/pip.log\"\n")
	write(fakes, "uv", `#!/bin/bash
echo "uv $* | BUILD_CONSTRAINT=$(cat "${UV_BUILD_CONSTRAINT:-/nonexistent}" 2>/dev/null)" >> "$MM_REC/uv.log"
mkpy() { mkdir -p "$1/bin"; cp "$MM_FAKES/venvpython" "$1/bin/python"; }
case "$1 $2" in
  "sync "*) mkpy "$UV_PROJECT_ENVIRONMENT" ;;
  "venv "*) for a in "$@"; do case "$a" in /*) mkpy "$a" ;; esac; done ;;
  "pip freeze") printf 'numpy==1.26.4\ntorch==2.8.0+cu128\n' ;;
esac
exit 0
`)
	// hf download: creates the files the installer checks for and every literal --include name.
	write(fakes, "hf", `#!/bin/bash
echo "hf $*" >> "$MM_REC/hf.log"
repo="$2"; shift 2
dest=""; cache=""; inc=()
while [ $# -gt 0 ]; do
  case "$1" in
    --revision) shift ;;
    --local-dir) dest="$2"; shift ;;
    --cache-dir) cache="$2"; shift ;;
    --include) shift; while [ $# -gt 0 ] && [ "${1#--}" = "$1" ]; do inc+=("$1"); shift; done; continue ;;
  esac
  shift
done
if [ -n "$cache" ]; then mkdir -p "$cache/models--${repo//\//--}"; exit 0; fi
mkdir -p "$dest"
case "$repo" in
  fishaudio/s2-pro) touch "$dest/codec.pth" ;;
  IndexTeam/*) touch "$dest/gpt.pth" ;;
esac
for f in ${inc[@]+"${inc[@]}"}; do
  case "$f" in *'*'*) continue ;; esac
  mkdir -p "$(dirname "$dest/$f")"; touch "$dest/$f"
done
`)
	bin, rec := fakeBin(t, map[string]string{
		"aws":     fakeAWS,
		"apt-get": "#!/bin/bash\necho \"apt-get $*\" >> \"$MM_REC/apt.log\"\n",
		// `python3 -m venv DIR` makes the tools venv (pip, uv, hf); anything else is the real python3.
		"python3": `#!/bin/bash
if [ "$1" = "-m" ] && [ "$2" = "venv" ]; then
  mkdir -p "$3/bin"; cp "$MM_FAKES/pip" "$MM_FAKES/uv" "$MM_FAKES/hf" "$3/bin/"; exit 0
fi
exec "$MM_REAL_PYTHON" "$@"
`,
		"git": `#!/bin/bash
echo "git $*" >> "$MM_REC/git.log"
case "$1" in
  clone) dest="${@: -1}"; mkdir -p "$dest"; case "$*" in *CosyVoice*) touch "$dest/.gitmodules" ;; esac ;;
  -C) d="$2"; shift 2
      case "$1" in
        checkout) echo "${@: -1}" > "$d/.fakehead" ;;
        rev-parse) cat "$d/.fakehead" ;;
      esac ;;
esac
exit 0
`,
		"df": `#!/bin/bash
case "$*" in
  *--output=pcent*) printf 'Use%%\n 41%%\n' ;;
  *) printf 'Filesystem Size Used Avail Use%% Mounted on\n/dev/root 194G 80G 114G 41%% /\n' ;;
esac
`,
	})
	work := t.TempDir()
	root, home, build := filepath.Join(work, "ai"), filepath.Join(work, "home"), filepath.Join(work, "build")
	script := filepath.Join(scriptsDir(t), "provision-lab-voice.sh")
	cmd := exec.Command("bash", "-c", `. "`+script+`"; lab_voice_install; echo "INSTALL-DONE manifest=$RUNTIME_MANIFEST"`)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "MM_REC="+rec, "MM_FAKES="+fakes, "MM_REAL_PYTHON="+realPy,
		"MM_PROVISION_LIB_ONLY=1", "MM_ALLOW_NONROOT=1", "MM_DATE=2026-11-11",
		"MM_ROOT="+root, "MM_HOME="+home, "LAB_BUILD_DIR="+build,
		"MM_EXPORT_MANIFEST="+filepath.Join(work, "export-manifest.json"),
		"MM_PROVISION_LOG="+filepath.Join(rec, "provision.log"),
		// The cleanup step removes $HOME/.cache/uv and pip: never the developer's own.
		"HOME="+filepath.Join(work, "userhome"))
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil || !strings.Contains(got, "INSTALL-DONE manifest=") {
		t.Fatalf("the install sequence did not run to the end: %v\n%s\nfailed: %s", err, got, recorded(rec, "s3/lab-voice.failed"))
	}
	if f := recorded(rec, "s3/lab-voice.failed"); f != "" {
		t.Fatalf("the build was marked failed: %s\n%s", f, got)
	}

	// One complete version record per model.
	type versions struct {
		Model           string   `json:"model"`
		Repo            string   `json:"repo"`
		RepoCommit      string   `json:"repoCommit"`
		PipFreezeSha256 string   `json:"pipFreezeSha256"`
		Python          string   `json:"python"`
		Torch           string   `json:"torch"`
		Weights         []string `json:"weights"`
		BuiltAt         string   `json:"builtAt"`
	}
	weightRef := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	wantPy := map[string]string{"fish_s2": "3.12", "indextts2": "3.11", "seedvc": "3.10", "cosyvoice3": "3.10"}
	for model, py := range wantPy {
		b, err := os.ReadFile(filepath.Join(root, "models", "lab-voice", model, "versions.json"))
		if err != nil {
			t.Errorf("%s: no versions.json: %v", model, err)
			continue
		}
		var v versions
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s: versions.json is not JSON: %v", model, err)
			continue
		}
		if v.Model != model || len(v.RepoCommit) != 40 || len(v.PipFreezeSha256) != 64 || v.Python != py ||
			v.Torch != "2.8.0+cu128" || len(v.Weights) == 0 || !strings.HasPrefix(v.Repo, "https://github.com/") {
			t.Errorf("%s: incomplete version record: %+v", model, v)
		}
		if v.BuiltAt != "" {
			t.Errorf("%s: builtAt must not be in the hashed record", model)
		}
		for _, w := range v.Weights {
			if !weightRef.MatchString(w) {
				t.Errorf("%s: weight %q is not repo@revision", model, w)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "venvs", "lab-voice-"+model, "bin", "python")); err != nil {
			t.Errorf("%s: no venv", model)
		}
		if _, err := os.Stat(filepath.Join(root, "repos", "lab-voice", model)); err != nil {
			t.Errorf("%s: no repo", model)
		}
	}
	// The manifest is a sha256, uploaded for the builder script, and equals what the script computes again.
	man := strings.TrimSpace(recorded(rec, "s3/lab-voice.runtime-manifest"))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(man) || !strings.Contains(got, "manifest="+man) {
		t.Errorf("runtime manifest %q not recorded:\n%s", man, got)
	}
	again, err := exec.Command(realPy, filepath.Join(root, "scripts", "lab", "runtime_manifest.py"),
		"--weights", filepath.Join(root, "models", "lab-voice"), "--wrappers", filepath.Join(root, "scripts", "lab")).Output()
	if err != nil || strings.TrimSpace(string(again)) != man {
		t.Errorf("manifest is not reproducible: %q vs %q (%v)", strings.TrimSpace(string(again)), man, err)
	}
	// Wrappers installed, build leftovers gone, the pinned checkout and the submodule step ran.
	if _, err := os.Stat(filepath.Join(root, "scripts", "lab", "voice_fish_s2.py")); err != nil {
		t.Error("wrappers not installed")
	}
	if _, err := os.Stat(build); err == nil {
		t.Error("the build directory (uv cache + tools) was left in the image")
	}
	gitLog := recorded(rec, "git.log")
	for _, want := range []string{"0f8a12e8a651062fdee9c33a894bdccd3a8513f1", "d9e41aac89fd00b3d71497fddb287b7f24613712",
		"51383efd921027683c89e5348211d93ff12ac2a8", "074ca6dc9e80a2f424f1f74b48bdd7d3fea531cc", "submodule update"} {
		if !strings.Contains(gitLog, want) {
			t.Errorf("git log lacks %q", want)
		}
	}
	// Every weight download is pinned to a 40-hex revision, and none carries a token.
	pinned := regexp.MustCompile(`--revision [0-9a-f]{40} `)
	for _, line := range strings.Split(strings.TrimSpace(recorded(rec, "hf.log")), "\n") {
		if !pinned.MatchString(line) || strings.Contains(line, "--token") {
			t.Errorf("unpinned or token-carrying download: %s", line)
		}
	}
	// Source builds are held to setuptools < 81 (openai-whisper 20231117 needs pkg_resources),
	// and the two hand-built venvs carry setuptools at run time.
	uvLog := recorded(rec, "uv.log")
	for _, line := range strings.Split(strings.TrimSpace(uvLog), "\n") {
		if (strings.HasPrefix(line, "uv sync") || strings.HasPrefix(line, "uv pip install")) && !strings.Contains(line, "BUILD_CONSTRAINT=setuptools<81") {
			t.Errorf("an install ran without the build constraint: %s", line)
		}
	}
	if n := strings.Count(uvLog, " setuptools<81 "); n != 2 {
		t.Errorf("setuptools<81 is in %d dependency installs, want 2 (seedvc, cosyvoice3)", n)
	}
	// Both Seed-VC warm-up conversions ran through the installed wrapper.
	if n := strings.Count(recorded(rec, "venvpython.log"), "voice_seedvc.py"); n != 2 {
		t.Errorf("seed-vc warm-up runs = %d, want 2", n)
	}
	// The status object keeps every line: the first step is still in it at the end.
	st := recorded(rec, "s3/lab-voice.status")
	if !strings.Contains(st, "tools: uv") || !strings.Contains(st, "runtime manifest "+man) {
		t.Errorf("status history incomplete:\n%s", st)
	}
}

// Every jq program in the burst scripts may only read variables it was given
// (--arg / --argjson / --slurpfile). This is the exact shape of the 2026-11-11 typo.
func TestBurstScriptsJqProgramsUseOnlyDeclaredVariables(t *testing.T) {
	declared := regexp.MustCompile(`--(?:arg|argjson|slurpfile|rawfile)\s+([A-Za-z_]\w*)`)
	used := regexp.MustCompile(`\$([A-Za-z_]\w*)`)
	program := regexp.MustCompile(`'([^']*)'`)
	jqCall := regexp.MustCompile(`(^|[\s;|(])jq\s`)
	var files []string
	for _, pat := range []string{"*.sh", "smoke/*.sh"} {
		m, _ := filepath.Glob(filepath.Join(scriptsDir(t), pat))
		files = append(files, m...)
	}
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Join continuation lines so one jq call is one statement.
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		for i, line := range strings.Split(text, "\n") {
			tl := strings.TrimSpace(line)
			idx := jqCall.FindStringIndex(tl)
			if idx == nil || strings.HasPrefix(tl, "#") {
				continue
			}
			call := tl[idx[0]:]
			if !strings.Contains(call, "--arg") && !strings.Contains(call, "--slurpfile") {
				continue
			}
			have := map[string]bool{"ENV": true, "__loc__": true, "__prog_args": true}
			for _, m := range declared.FindAllStringSubmatch(call, -1) {
				have[m[1]] = true
			}
			for _, p := range program.FindAllStringSubmatch(call, -1) {
				for _, u := range used.FindAllStringSubmatch(p[1], -1) {
					// `as $x` bindings declare their own names.
					if strings.Contains(p[1], "as $"+u[1]) {
						continue
					}
					if !have[u[1]] {
						t.Errorf("%s (statement %d): jq reads $%s but it was never passed with --arg: %s", filepath.Base(f), i+1, u[1], call)
					}
				}
			}
			checked++
		}
	}
	if checked < 5 {
		t.Fatalf("only %d jq calls with arguments were checked; the scan is broken", checked)
	}
}
