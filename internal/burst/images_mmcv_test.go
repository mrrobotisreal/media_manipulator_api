package burst

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// On 2026-11-11 a restore builder compiled mmcv for 40 minutes and then failed
// with "mmcv _ext has no sm_89 code". The check sent cuobjdump's own errors to
// /dev/null, so a missing or failing tool and missing code read the same, and
// the same build settings were later shown on the server to produce both
// architectures. Since then the two-architecture mmcv is built and checked
// once on the server (build-mmcv-wheel.sh) and the builder installs that wheel
// by sha256; an architecture check names what it found.

const mmcvCommit = "57c4e25e06e2d4f8a9357c84bcd24089a284dc88"
const mmcvKeyBase = "mmcv-2.1.0-torch2.11.0_cu128-cp312-sm_89-sm_120"

// fakeCuobjdump lists one ELF per architecture in $MM_FAKE_ARCHS, like the real tool.
const fakeCuobjdump = `#!/bin/bash
[ "${MM_FAKE_CUOBJDUMP_FAIL:-0}" = 1 ] && { echo "cuobjdump: cannot open file" >&2; exit 1; }
i=0
for a in ${MM_FAKE_ARCHS:-}; do i=$((i+1)); echo "ELF file    $i: _ext.cpython-312-x86_64-linux-gnu.$i.$a.cubin"; done
`

// fakeVenvPython stands in for the restore venv's python.
const fakeVenvPython = `#!/bin/bash
echo "python $*" >> "$MM_REC/python.log"
case "$*" in
  *"import torch; print"*) echo "2.11.0+cu128" ;;
  *"sys.version_info"*) echo "cp312" ;;
  *"import mmcv, mmcv.ops"*) echo "mmcv 2.1.0" ;;
  *"import mmcv,os"*) echo "$MM_REC/_ext.cpython-312-x86_64-linux-gnu.so" ;;
  *"pip wheel"*)
    out=""; prev=""
    for a in "$@"; do [ "$prev" = "-w" ] && out="$a"; prev="$a"; done
    [ "${MM_FAKE_BUILD_FAIL:-0}" = 1 ] && { echo "error: nvcc failed" >&2; exit 1; }
    "$MM_REAL_PYTHON" - "$out" <<'PY'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1] + "/mmcv-2.1.0-cp312-cp312-linux_x86_64.whl", "w") as z:
    z.writestr("mmcv/_ext.cpython-312-x86_64-linux-gnu.so", "compiled")
    z.writestr("mmcv/__init__.py", "")
PY
    ;;
esac
exit 0
`

func mmcvRig(t *testing.T) (bin, rec, root, cuda string) {
	t.Helper()
	bin, rec = fakeBin(t, map[string]string{"aws": fakeAWS})
	work := t.TempDir()
	root, cuda = filepath.Join(work, "ai"), filepath.Join(work, "cuda")
	for _, d := range []string{filepath.Join(root, "venvs", "restore-vsr-mm", "bin"), filepath.Join(cuda, "bin"), filepath.Join(rec, "serve")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(root, "venvs", "restore-vsr-mm", "bin", "python"): fakeVenvPython,
		filepath.Join(cuda, "bin", "cuobjdump"):                         fakeCuobjdump,
		filepath.Join(cuda, "bin", "nvcc"):                              "#!/bin/bash\nexit 0\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, rec, root, cuda
}

// The builder installs the server-built wheel by sha256 and compiles nothing.
func TestStepRebuildMmcvInstallsTheServerBuiltWheel(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	wheel := []byte("a wheel")
	sum := sha256.Sum256(wheel)
	good := hex.EncodeToString(sum[:])
	record := func(sha, commit string) string {
		return `{"wheel":"mmcv-2.1.0-cp312-cp312-linux_x86_64.whl","sha256":"` + sha + `","commit":"` + commit + `","torch":"2.11.0+cu128","python":"cp312"}`
	}
	cases := []struct {
		name     string
		record   string // "" = not in S3
		env      []string
		dropTool bool
		wantOK   bool
		want     []string
	}{
		{name: "the wheel is installed and both architectures are listed", record: record(good, mmcvCommit), env: []string{"MM_FAKE_ARCHS=sm_89 sm_120"},
			wantOK: true, want: []string{"mmcv _ext: CUDA code for sm_120 sm_89"}},
		{name: "no wheel in S3: says how to make one", wantOK: false,
			want: []string{"there is no two-architecture mmcv build in S3 for torch 2.11.0+cu128 / cp312", "scripts/burst/build-mmcv-wheel.sh"}},
		{name: "a wheel that is not the recorded one is refused", record: record(strings.Repeat("0", 64), mmcvCommit), wantOK: false, want: []string{"mmcv wheel sha256 mismatch"}},
		{name: "a wheel of another commit is refused", record: record(good, strings.Repeat("a", 40)), wantOK: false, want: []string{"not " + mmcvCommit}},
		{name: "one architecture missing: names what is there", record: record(good, mmcvCommit), env: []string{"MM_FAKE_ARCHS=sm_120"},
			wantOK: false, want: []string{"mmcv _ext has no CUDA code for: sm_89 (it has: sm_120"}},
		{name: "cuobjdump not on the image: a note, not a failure", record: record(good, mmcvCommit), dropTool: true,
			wantOK: true, want: []string{"were not listed here (cuobjdump is not on this machine)", "the wheel's sha256 matches the build that was checked on the server"}},
		{name: "cuobjdump fails: a note that says so, not 'no code'", record: record(good, mmcvCommit), env: []string{"MM_FAKE_CUOBJDUMP_FAIL=1"},
			wantOK: true, want: []string{"cuobjdump could not read it: cuobjdump: cannot open file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, rec, root, cuda := mmcvRig(t)
			if c.record != "" {
				_ = os.WriteFile(filepath.Join(rec, "serve", mmcvKeyBase+".json"), []byte(c.record), 0o644)
				_ = os.WriteFile(filepath.Join(rec, "serve", mmcvKeyBase+".whl"), wheel, 0o644)
			}
			if c.dropTool {
				_ = os.Remove(filepath.Join(cuda, "bin", "cuobjdump"))
			}
			env := append([]string{"MM_ROOT=" + root, "CUDA_HOME_DIR=" + cuda}, c.env...)
			out, err, _ := runCommon(t, bin, rec, "step_rebuild_mmcv", env...)
			said := recorded(rec, "s3/restore.status") + recorded(rec, "s3/restore.failed")
			if (err == nil) != c.wantOK {
				t.Fatalf("ok=%v want %v\n%s\n%s", err == nil, c.wantOK, out, said)
			}
			for _, w := range c.want {
				if !strings.Contains(said, w) {
					t.Errorf("missing %q in:\n%s", w, said)
				}
			}
			py := recorded(rec, "python.log")
			if strings.Contains(py, "pip wheel") || strings.Contains(py, "--no-build-isolation") || strings.Contains(recorded(rec, "aws.log"), "git") {
				t.Errorf("the builder must not compile mmcv:\n%s", py)
			}
			if c.wantOK && !strings.Contains(py, "pip install --no-index --no-deps --force-reinstall") {
				t.Errorf("the wheel was not installed offline:\n%s", py)
			}
			if !c.wantOK && strings.Contains(py, "pip install") && !strings.Contains(c.name, "architecture") {
				t.Errorf("a refused wheel was installed:\n%s", py)
			}
		})
	}
}

// build-mmcv-wheel.sh: compiles once, refuses a build that lacks an
// architecture, records what it built, and reuses a finished build.
func TestBuildMmcvWheelChecksBothArchitecturesBeforeItRecords(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "sha256sum", "unzip", "nice"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	realPy, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	script := filepath.Join(scriptsDir(t), "build-mmcv-wheel.sh")
	run := func(t *testing.T, bin, rec, root, cuda, stage string, env ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", script, "--no-upload", "--jobs", "2")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_REC="+rec, "MM_REAL_PYTHON="+realPy,
			"MM_ROOT="+root, "CUDA_HOME_DIR="+cuda, "MM_MMCV_STAGE="+stage)
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	git := `#!/bin/bash
echo "git $*" >> "$MM_REC/git.log"
case "$*" in
  *clone*) mkdir -p "${@: -1}" ;;
  *rev-parse*) echo "${MM_FAKE_HEAD:-` + mmcvCommit + `}" ;;
esac
`
	newRig := func(t *testing.T) (bin, rec, root, cuda, stage string) {
		bin, rec, root, cuda = mmcvRig(t)
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin, rec, root, cuda, t.TempDir()
	}

	t.Run("both architectures: recorded, and reused on the next run", func(t *testing.T) {
		bin, rec, root, cuda, stage := newRig(t)
		out, err := run(t, bin, rec, root, cuda, stage, "MM_FAKE_ARCHS=sm_120 sm_89")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, want := range []string{"MMCV WHEEL RESULT", "built for:       torch 2.11.0+cu128, cp312", "architectures:   sm_120 sm_89", "not uploaded (kept at "} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		b, err := os.ReadFile(filepath.Join(stage, mmcvKeyBase, "wheel.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"commit": "` + mmcvCommit + `"`, `"torch": "2.11.0+cu128"`, `"python": "cp312"`, `"sm_89"`, `"sm_120"`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("record lacks %s:\n%s", want, b)
			}
		}
		if !regexp.MustCompile(`"sha256": "[0-9a-f]{64}"`).Match(b) {
			t.Errorf("record has no sha256:\n%s", b)
		}
		// The build ran with both architectures requested, at low priority, without touching the venv.
		py := recorded(rec, "python.log")
		if !strings.Contains(py, "pip wheel --no-build-isolation --no-deps") {
			t.Errorf("unexpected build call:\n%s", py)
		}
		if strings.Contains(py, "pip install") || strings.Contains(py, "pip uninstall") {
			t.Errorf("the server's venv must never be installed into:\n%s", py)
		}
		out2, err := run(t, bin, rec, root, cuda, stage, "MM_FAKE_ARCHS=sm_120 sm_89")
		if err != nil || !strings.Contains(out2, "reusing the finished build") {
			t.Fatalf("second run: %v\n%s", err, out2)
		}
		if n := strings.Count(recorded(rec, "git.log"), "clone"); n != 1 {
			t.Errorf("clones = %d, want 1 (the second run reuses the build)", n)
		}
	})
	for _, c := range []struct {
		name, want string
		env        []string
	}{
		{"one architecture missing", "the build has no CUDA code for sm_89 (it has: sm_120", []string{"MM_FAKE_ARCHS=sm_120"}},
		{"cuobjdump fails", "cuobjdump is missing or could not read the build", []string{"MM_FAKE_CUOBJDUMP_FAIL=1"}},
		{"the compile fails", "the mmcv build failed", []string{"MM_FAKE_BUILD_FAIL=1"}},
		{"the tag moved", "is not commit " + mmcvCommit, []string{"MM_FAKE_HEAD=" + strings.Repeat("b", 40)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, rec, root, cuda, stage := newRig(t)
			out, err := run(t, bin, rec, root, cuda, stage, c.env...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Fatalf("must be refused with %q: %v\n%s", c.want, err, out)
			}
			if strings.Contains(out, "MMCV WHEEL RESULT") {
				t.Errorf("printed a result box:\n%s", out)
			}
			if _, e := os.Stat(filepath.Join(stage, mmcvKeyBase, "wheel.json")); e == nil {
				t.Error("a record was written for a refused build")
			}
		})
	}
}

// build-image.sh refuses to launch a restore builder when the mmcv wheel is
// not in S3: the builder would otherwise stop after unpacking 34 GB.
func TestBuildImageRestoreNeedsTheMmcvWheelBeforeLaunch(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	_, rec, root, _ := mmcvRig(t)
	bin := t.TempDir()
	fake := `#!/bin/sh
echo "$*" >> "$MM_REC/aws.log"
case "$*" in
  *"s3api head-object"*wheels/*) exit 1 ;;
  *"s3api head-object"*) exit 0 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *) echo "UNEXPECTED: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--yes", "--instance-type", "g7.2xlarge")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_REC="+rec, "MM_ROOT="+root)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err == nil {
		t.Fatalf("must stop:\n%s", got)
	}
	for _, want := range []string{"the two-architecture mmcv build is not in S3 yet", "burst-images/wheels/" + mmcvKeyBase + ".json", "scripts/burst/build-mmcv-wheel.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(recorded(rec, "aws.log"), "run-instances") || strings.Contains(got, "UNEXPECTED") {
		t.Errorf("a builder was launched or an unexpected call was made:\n%s", got)
	}
}

// No provisioning or smoke script may read a bare $HOME: the builder's
// user-data runs under cloud-init with HOME unset, and `set -u` ends the build.
func TestBuilderScriptsNeverReadABareHome(t *testing.T) {
	bare := regexp.MustCompile(`\$HOME\b|\$\{HOME\}`)
	var files []string
	for _, pat := range []string{"provision-*.sh", "smoke/*.sh"} {
		m, _ := filepath.Glob(filepath.Join(scriptsDir(t), pat))
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("only %d scripts found", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			tl := strings.TrimSpace(line)
			if strings.HasPrefix(tl, "#") {
				continue
			}
			// smoke scripts set HOME on their first lines (with a default); after that line it is set.
			if strings.HasPrefix(tl, "export HOME=") {
				if bare.MatchString(tl) {
					t.Errorf("%s:%d sets HOME from a bare $HOME: %s", filepath.Base(f), i+1, tl)
				}
				continue
			}
			if bare.MatchString(tl) && strings.HasPrefix(filepath.Base(f), "provision-") {
				t.Errorf("%s:%d reads $HOME, which is unset on the builder (use BUILDER_HOME or MM_HOME): %s", filepath.Base(f), i+1, tl)
			}
		}
	}
}

// A script error that is not a fail_build (an unset variable, a failing
// command) must still be reported as FAILED with its own last message. On
// 2026-11-12 the lab-voice build ended on "HOME: unbound variable" with no
// FAILED line, and the person reading the log could not tell it had failed.
func TestProvisionReportsAScriptErrorAsFailed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	steps := `step_base_packages() { :; }; step_ffmpeg() { :; }; step_tailscale() { :; }; step_unpack() { :; }; step_vulkan_driver_bits() { :; }
step_dirs_and_unit() { :; }; step_image_json() { :; }; step_smoke() { :; }; step_cleanup() { :; }; step_ready_and_wait() { echo REACHED-READY; }
`
	for _, c := range []struct {
		name, rebuild string
		wantFail      bool
		want          []string
		notWant       string
	}{
		{"an unset variable", `oops() { echo "about to read it" >&2; echo "$MM_NOT_SET_ANYWHERE"; }`, true,
			[]string{"FAILED: the build script itself stopped", "this is a bug in the script", "MM_NOT_SET_ANYWHERE"}, ""},
		{"a failing command", `oops() { echo "copying" >&2; cp /nonexistent/a /nonexistent/b; }`, true,
			[]string{"FAILED: the build script itself stopped"}, ""},
		{"a fail_build is reported once, in its own words", `oops() { fail_build "smoke test failed"; }`, true,
			[]string{"FAILED: smoke test failed"}, "the build script itself stopped"},
		{"a fail_build inside a subshell is still reported once", `oops() { ( exit 1 ) || fail_build "mmcv build failed"; }`, true,
			[]string{"FAILED: mmcv build failed"}, "the build script itself stopped"},
		{"a clean run reports no failure", `oops() { :; }`, false, nil, "FAILED"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, rec := fakeBin(t, map[string]string{"aws": fakeAWS})
			log := filepath.Join(rec, "provision.log")
			// Like the builder's user-data: the script's own output is appended to the provision log.
			out, err, _ := runCommon(t, bin, rec, steps+c.rebuild+`; provision_main oops >> "`+log+`" 2>&1`, "MM_FAILED_MARK="+filepath.Join(rec, "failed.mark"))
			st := recorded(rec, "s3/restore.status")
			if (err != nil) != c.wantFail {
				t.Fatalf("failed=%v want %v\n%s\n%s", err != nil, c.wantFail, out, st)
			}
			for _, w := range c.want {
				if !strings.Contains(st, w) {
					t.Errorf("status lacks %q:\n%s", w, st)
				}
			}
			if c.notWant != "" && strings.Contains(st, c.notWant) {
				t.Errorf("status must not say %q:\n%s", c.notWant, st)
			}
			if n := strings.Count(st, "FAILED:"); c.wantFail && n != 1 {
				t.Errorf("FAILED lines = %d, want exactly 1:\n%s", n, st)
			}
			if b, _ := os.ReadFile(log); c.wantFail == strings.Contains(string(b), "REACHED-READY") {
				t.Errorf("ready step reached=%v on a build with wantFail=%v", !c.wantFail, c.wantFail)
			}
		})
	}
}

// When the builder reports a failure, build-image.sh ends with a box that
// states the reason in one line, so a failure cannot be mistaken for a pause.
func TestBuildImageEndsAFailedBuildWithTheReason(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	bin, rec := t.TempDir(), t.TempDir()
	fake := `#!/bin/sh
case "$1 $2" in
  "ec2 run-instances") echo i-0builder; exit 0 ;;
  "ec2 describe-instances") printf 'running\tNone\n'; exit 0 ;;
  "ec2 terminate-instances") echo "$*" > "$MM_REC/terminated"; exit 0 ;;
esac
case "$*" in
  *"s3api head-object"*failed*) exit 0 ;;
  *"s3api head-object"*) exit 1 ;;
  *"s3 cp s3://"*".status -"*) printf '15:29:20 cleanup: uv cache\n15:29:21 FAILED: the build script itself stopped (exit 1); this is a bug in the script, not a failed check. Its last message: line 268: HOME: unbound variable\n' ;;
  *"s3 cp s3://"*".log -"*) echo "some log line" ;;
  *"s3 cp s3://"*"smoke.json -"*) exit 1 ;;
  *"s3 cp "*|*"s3 rm "*) exit 0 ;;
  *"ssm get-parameter"*) echo ami-0e5b7b1a57b07ca51 ;;
  *"Images[0].Name"*) echo "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106" ;;
  *"Images[0].RootDeviceName"*) echo /dev/sda1 ;;
  *"Ebs.VolumeSize"*) echo 75 ;;
  *"Images[0].Architecture"*) echo x86_64 ;;
  *) echo "UNEXPECTED: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "lab-voice", "2026-11-12", "--yes")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_POLL_SECONDS=1", "MM_REC="+rec)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err == nil {
		t.Fatalf("must fail:\n%s", got)
	}
	for _, want := range []string{"BUILD FAILED (paste from here up into chat)", "REASON: 15:29:21 FAILED: the build script itself stopped", "HOME: unbound variable",
		"No image was made", "terminating builder i-0builder"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "some log line"), strings.Index(got, "REASON:"); i < 0 || j < i {
		t.Errorf("the reason must come after the log lines:\n%s", got)
	}
	if strings.Contains(got, "UNEXPECTED") {
		t.Errorf("unexpected aws call:\n%s", got)
	}
}
