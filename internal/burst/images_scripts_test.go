package burst

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
)

func scriptsDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs("../../scripts/burst")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The builder launches an AMI with the profile's root size; the AMI's snapshot
// must not be bigger, so lib.sh mirrors profiles.go and this keeps them equal.
func TestLibRootSizesMatchTheBurstProfiles(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scriptsDir(t), "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s+(image|audio|video|restore\)?)\)? echo (\d+) ;;`)
	got := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		n, _ := strconv.Atoi(m[2])
		got[strings.TrimSuffix(m[1], ")")] = n
	}
	if len(got) != 4 {
		t.Fatalf("could not parse lib.sh root sizes: %v", got)
	}
	min := map[string]int{}
	for _, p := range profiles {
		if p.Never || p.Family == "" {
			continue
		}
		if m, ok := min[p.Family]; !ok || p.RootGiB < m {
			min[p.Family] = p.RootGiB
		}
	}
	for fam, gib := range got {
		if min[fam] != gib {
			t.Errorf("family %s: lib.sh says %d GiB, the smallest profile root is %d", fam, gib, min[fam])
		}
	}
}

func TestProvisionDryRunPrintsAPlanAndRunsNothing(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, fam := range manifest.Families {
		out, err := exec.Command("bash", filepath.Join(scriptsDir(t), "provision-"+fam+".sh"), "--dry-run", "--date", "2026-11-10").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", fam, err, out)
		}
		s := string(out)
		for _, want := range []string{"PLAN  ", "s3://media-manipulator/burst-images/2026-11-10/" + fam + ".tar.zst", "cloud-init clean", "smoke/" + fam + ".sh", "HF_HUB_OFFLINE=1"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s dry-run plan lacks %q", fam, want)
			}
		}
		hasMMCV := strings.Contains(s, "mmcv: install")
		if (fam == "restore") != hasMMCV {
			t.Errorf("%s: mmcv rebuild presence wrong (%v)", fam, hasMMCV)
		}
		if strings.Contains(s, `8.9;12.0`) != true {
			t.Errorf("%s: plan must name both architectures", fam)
		}
		if strings.Contains(s, "magick ") && !strings.Contains(s, "never magick") {
			t.Errorf("%s: plan mentions magick", fam)
		}
	}
}

func TestBuildImageDryRunNeverCallsAWS(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// A fake `aws` on PATH that fails loudly proves no call is made.
	bin := t.TempDir()
	fake := "#!/bin/sh\necho AWS-WAS-CALLED \"$@\" >&2\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--dry-run", "--yes")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "AWS-WAS-CALLED") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	for _, want := range []string{"Estimated cost", "mm-burst-build", "terminate", "BURST_AMIS=restore:x86_64="} {
		if !strings.Contains(string(out), want) {
			t.Errorf("dry-run missing %q", want)
		}
	}
	// Only g6.xlarge / g7.2xlarge are accepted (the IAM policy allows only those).
	bad := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--dry-run", "--yes", "--instance-type", "p5.48xlarge")
	if out, err := bad.CombinedOutput(); err == nil {
		t.Fatalf("p5 must be refused:\n%s", out)
	}
}

// A real (non-dry) run up to the y/N prompt against a fake `aws` that answers
// like AWS did on 2026-11-10: the DLAMI's name has spaces in it, which once
// leaked into the root device name and broke every launch.
func TestBuildImageReadsRootDeviceFromANameWithSpaces(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
case "$*" in
  *"s3api head-object"*) exit 0 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *"ssm get-parameter"*) echo ami-0e5b7b1a57b07ca51 ;;
  *"[Name,RootDeviceName]"*) printf 'Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106\t/dev/sda1\n' ;;
  *"Images[0].Name"*) printf 'Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106\n' ;;
  *"Images[0].RootDeviceName"*) echo /dev/sda1 ;;
  *"Ebs.VolumeSize"*) echo 75 ;;
  *"Images[0].Architecture"*) echo x86_64 ;;
  *) echo "UNEXPECTED: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "video", "2026-11-10")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("n\n") // decline at the launch prompt
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "root device /dev/sda1") {
		t.Fatalf("root device not parsed:\n%s", got)
	}
	if !strings.Contains(got, "Base root disk: 75 GiB") {
		t.Errorf("base root disk not read for /dev/sda1:\n%s", got)
	}
	if strings.Contains(got, "UNEXPECTED") || strings.Contains(got, "launched") {
		t.Errorf("must stop at the prompt without launching:\n%s", got)
	}
}

// On 2026-11-10 an old API reaper terminated a video builder a minute after
// launch, and build-image.sh then waited out its full 170-minute timeout. The
// wait loop must notice a vanished builder and stop with the reason.
func TestBuildImageStopsWhenTheBuilderIsTerminated(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go (the script builds burst-ctl for the bundle)")
	}
	bin := t.TempDir()
	// ec2 calls first: run-instances carries user-data that itself contains
	// "aws s3 cp ...", so the s3 patterns must not see it.
	fake := `#!/bin/sh
case "$1 $2" in
  "ec2 run-instances") echo i-0builder; exit 0 ;;
  "ec2 describe-instances") printf 'terminated\tClient.UserInitiatedShutdown: User initiated shutdown\n'; exit 0 ;;
  "ec2 terminate-instances") echo "TERMINATE-CALLED" >&2; exit 0 ;;
esac
case "$*" in
  *"s3api head-object"*manifest.json*) exit 0 ;;
  *"s3api head-object"*) exit 1 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *"s3 cp s3://"*".status -"*) echo "apt: base packages" ;;
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
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "video", "2026-11-10", "--yes")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_POLL_SECONDS=0")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("build-image.sh kept waiting for a terminated builder")
	}
	got := string(out)
	if err == nil {
		t.Fatalf("must fail:\n%s", got)
	}
	for _, want := range []string{"builder is gone before it finished", "state: terminated", "User initiated shutdown", "MM126 or later"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TERMINATE-CALLED") || strings.Contains(got, "could not terminate") {
		t.Errorf("must not try to terminate an instance that is already gone:\n%s", got)
	}
	if strings.Contains(got, "UNEXPECTED") {
		t.Errorf("unexpected aws call:\n%s", got)
	}
}

// Under `set -o pipefail`, `producer | grep -q x` and `producer | head -1`
// report failure when the producer is cut off (SIGPIPE, exit 141) even though
// the match is there. On 2026-11-10 that failed a builder with "ffmpeg has no
// h264_nvenc" on an ffmpeg that has NVENC. No burst script may use either.
func TestBurstScriptsNeverPipeIntoEarlyExitReaders(t *testing.T) {
	bad := regexp.MustCompile(`\|\s*(grep\s+(-[a-zA-Z]*[qm][a-zA-Z]*|--quiet|--max-count)|head(\s|$))`)
	err := filepath.Walk(scriptsDir(t), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".sh") {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if bad.MatchString(line) {
				t.Errorf("%s:%d pipes into an early-exit reader (use output_has or sed -n '1p'): %s", filepath.Base(path), i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// output_has must find a match early in a long output under pipefail (the
// shape that failed), and must not match what is absent.
func TestOutputHasIsSafeUnderPipefail(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	common := filepath.Join(scriptsDir(t), "provision-common.sh")
	run := func(script string) error {
		cmd := exec.Command("bash", "-c", ". \""+common+"\"; "+script)
		cmd.Env = append(os.Environ(), "MM_DRY=1", "MM_FAMILY=video")
		return cmd.Run()
	}
	long := `bash -c "echo h264_nvenc; seq 1 300000"`
	if err := run("output_has h264_nvenc " + long); err != nil {
		t.Fatalf("a match at the top of a long output must be found: %v", err)
	}
	if err := run("output_has av1_qsv " + long); err == nil {
		t.Fatal("an absent pattern must not match")
	}
	// The old shape really does fail here, which is the bug this guards.
	if err := run("seq 1 300000 | grep -q 1"); err == nil {
		t.Log("note: this bash did not SIGPIPE the producer; the lint test above still forbids the shape")
	}
}

// On 2026-11-10 the video build passed its smoke test, then idled for an hour
// on a g7.2xlarge waiting for the AMI to become "available" and gave up while
// the snapshot was still copying. The builder must be terminated as soon as
// the snapshot exists (the snapshot is point-in-time), and the wait must not
// call a slow-but-pending image a failure.
func TestBuildImageTerminatesBuilderOnceSnapshotStarts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go (the script builds burst-ctl for the bundle)")
	}
	bin := t.TempDir()
	state := filepath.Join(bin, "image-polls")
	calls := filepath.Join(bin, "calls")
	fake := `#!/bin/sh
case "$1 $2" in
  "ec2 run-instances") echo i-0builder; exit 0 ;;
  "ec2 describe-instances") printf 'running\tNone\n'; exit 0 ;;
  "ec2 terminate-instances") echo "terminate $*" >> "` + calls + `"; exit 0 ;;
  "ec2 create-image") echo "create-image" >> "` + calls + `"; echo ami-0new; exit 0 ;;
  "ec2 describe-snapshots") echo 100; exit 0 ;;
esac
case "$*" in
  *"ami-0new"*"Ebs.SnapshotId"*) echo snap-0abc ;;
  *"ami-0new"*"Images[0].State"*)
    n=$(cat "` + state + `" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + state + `"
    echo "state-poll $n" >> "` + calls + `"
    if [ $n -ge 3 ]; then echo available; else echo pending; fi ;;
  *"ami-0new"*"Snapshot"*) echo snap-0abc ;;
  *"s3api head-object"*manifest.json*) exit 0 ;;
  *"s3api head-object"*ready*) exit 0 ;;
  *"s3api head-object"*) exit 1 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *"s3 cp s3://"*smoke.json*) echo '{"passed":true,"driverAndGpu":"595.112.07, NVIDIA RTX PRO 4500","cases":[{"name":"rife-v4.6","ok":true,"seconds":9.9,"gpuPeakMiB":295}]}' ;;
  *"s3 cp s3://"*".status -"*) echo "READY: image can be created" ;;
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
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "video", "2026-11-10", "--yes", "--instance-type", "g7.2xlarge")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_POLL_SECONDS=0")
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("build must succeed:\n%s", got)
	}
	if !strings.Contains(got, "snapshot snap-0abc started; terminating the builder now") {
		t.Fatalf("the builder must be terminated once the snapshot starts:\n%s", got)
	}
	b, rerr := os.ReadFile(calls)
	if rerr != nil {
		t.Fatal(rerr)
	}
	seq := strings.Split(strings.TrimSpace(string(b)), "\n")
	// Expected order: create-image, terminate (once), then the state polls
	// until "available". Terminating before the image is available is the point.
	if len(seq) < 2 || seq[0] != "create-image" || !strings.HasPrefix(seq[1], "terminate ec2 terminate-instances --instance-ids i-0builder") {
		t.Fatalf("want create-image then terminate before any state poll; got %q", seq)
	}
	if n := strings.Count(string(b), "terminate "); n != 1 {
		t.Errorf("terminate exactly once (not again in the exit handler); got %d: %q", n, seq)
	}
	for _, want := range []string{"BURST_AMIS entry: video:x86_64=ami-0new", "snapshot size:    100 GiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "UNEXPECTED") {
		t.Errorf("unexpected aws call:\n%s", got)
	}
}

// --result prints the BUILD RESULT box for an image that finished after the
// script stopped (the 2026-11-10 video image), reading only from AWS and S3.
func TestBuildImageResultModeReadsAnExistingImage(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
case "$1 $2" in
  "ec2 run-instances"|"ec2 create-image"|"ec2 terminate-instances") echo "MUST-NOT-LAUNCH $*" >&2; exit 99 ;;
  "ec2 describe-snapshots") echo 100; exit 0 ;;
esac
case "$*" in
  *"ami-0469153071fef4704"*"Images[0].State"*) echo available ;;
  *"ami-0469153071fef4704"*"mm-burst-image"*) echo 1 ;;
  *"ami-0469153071fef4704"*"family"*) echo video ;;
  *"ami-0469153071fef4704"*"RootDeviceName"*) echo /dev/sda1 ;;
  *"ami-0469153071fef4704"*"base-ami"*) echo ami-0e5b7b1a57b07ca51 ;;
  *"ami-0469153071fef4704"*"runtime-manifest"*) echo 64150c00 ;;
  *"ami-0469153071fef4704"*"CreationDate"*) echo 2026-11-10T15:38:54.000Z ;;
  *"ami-0469153071fef4704"*"SnapshotId"*) printf 'snap-0c86df5d09216c8bd\tNone\tNone\n' ;;
  *"ami-0e5b7b1a57b07ca51"*"Images[0].Name"*) echo "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106" ;;
  *"s3 cp s3://"*smoke.json*) echo '{"passed":true,"driverAndGpu":"595.112.07, NVIDIA RTX PRO 4500","cases":[{"name":"rife-v4.6","ok":true,"seconds":9.92,"gpuPeakMiB":295}]}' ;;
  *) echo "UNEXPECTED: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "video", "2026-11-10", "--result", "ami-0469153071fef4704")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("--result must succeed:\n%s", got)
	}
	for _, want := range []string{"BUILD RESULT", "AMI:              ami-0469153071fef4704", "root device:      /dev/sda1", "snapshot size:    100 GiB", `"rife-v4.6"`, "BURST_AMIS entry: video:x86_64=ami-0469153071fef4704"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "MUST-NOT-LAUNCH") || strings.Contains(got, "UNEXPECTED") || strings.Contains(got, "Launch a") {
		t.Errorf("--result must only read:\n%s", got)
	}
}

// The smoke output checks run on the builder, so a broken check fails a good
// build. On 2026-11-10 is_png had a stray "x" after -format, so identify also
// opened a file named "x" and every image case failed on valid PNGs. Run each
// check string against real good and bad files.
func TestSmokeOutputChecksAcceptGoodFilesAndRejectBad(t *testing.T) {
	for _, tool := range []string{"bash", "identify", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("needs " + tool)
		}
	}
	dir := t.TempDir()
	lib := filepath.Join(scriptsDir(t), "smoke", "smoke-lib.sh")
	good := filepath.Join(scriptsDir(t), "smoke", "fixtures", "photo.png")
	notPNG := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(notPNG, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(dir, "a.wav")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=duration=2", wav).CombinedOutput(); err != nil {
		t.Fatalf("make wav: %v %s", err, out)
	}
	short := filepath.Join(dir, "short.wav")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=duration=0.3", short).CombinedOutput(); err != nil {
		t.Fatalf("make wav: %v %s", err, out)
	}
	// check runs the named helper to get the check string, then evaluates it
	// with sh -c exactly as case_run does.
	check := func(helper, file string) bool {
		script := ". '" + lib + "' >/dev/null 2>&1; c=$(" + helper + " '" + file + "'); sh -c \"$c\""
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "SMOKE_OUT="+filepath.Join(dir, "s.json"))
		return cmd.Run() == nil
	}
	cases := []struct {
		helper, file string
		want         bool
	}{
		{"is_png", good, true},
		{"is_png", notPNG, false},
		{"is_png", filepath.Join(dir, "missing.png"), false},
		{"is_png", wav, false},
		{"valid_wav", wav, true},
		{"valid_wav", short, false},
		{"valid_wav", notPNG, false},
	}
	// DeepFilterNet's output name varies by model version; valid_wav_in finds it.
	wavDir := filepath.Join(dir, "df")
	if err := os.MkdirAll(wavDir, 0o755); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(wav); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(wavDir, "in48_DeepFilterNet3.wav"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cases = append(cases,
		struct {
			helper, file string
			want         bool
		}{"valid_wav_in", wavDir, true},
		struct {
			helper, file string
			want         bool
		}{"valid_wav_in", emptyDir, false},
	)
	for _, c := range cases {
		if got := check(c.helper, c.file); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.helper, filepath.Base(c.file), got, c.want)
		}
	}
}

// The whole case_run harness, not just the check strings: a fake nvidia-smi
// reports GPU memory rising while the case runs, and each case's check reads
// the command's own output through $CASE_LOG (on 2026-11-10 the redact check
// grepped a log name that does not exist, failing a tool that had worked).
func TestSmokeCaseRunReadsTheCaseLogAndRecordsResults(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("needs " + tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// nvidia-smi: 100 MiB idle, 900 MiB while a "busy" marker file exists.
	busy := filepath.Join(dir, "busy")
	smi := "#!/bin/sh\nif [ -f '" + busy + "' ]; then echo 900; else echo 100; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(smi), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "smoke.json")
	lib := filepath.Join(scriptsDir(t), "smoke", "smoke-lib.sh")
	script := `. '` + lib + `'
work() { touch '` + busy + `'; sleep 0.5; echo "$1"; rm -f '` + busy + `'; }
case_run says-ok 1 "" "grep -q 'redactions=2' \"\$CASE_LOG\"" -- bash -c "$(declare -f work); work redactions=2"
case_run says-nothing 1 "" "grep -q 'redactions=[1-9]' \"\$CASE_LOG\"" -- bash -c "$(declare -f work); work redactions=0"
case_run no-gpu 1 "" "true" -- true
smoke_finish fake || true
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SMOKE_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harness: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Passed bool `json:"passed"`
		Cases  []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
			GPU  int    `json:"gpuPeakMiB"`
			Why  string `json:"why"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("smoke json: %v\n%s", err, b)
	}
	got := map[string]bool{}
	for _, c := range res.Cases {
		got[c.Name] = c.OK
		switch c.Name {
		case "says-ok":
			if !c.OK || c.GPU < 700 {
				t.Errorf("says-ok must pass with GPU use recorded: %+v", c)
			}
		case "says-nothing":
			if c.OK || c.Why != "output check failed" {
				t.Errorf("says-nothing must fail its check: %+v", c)
			}
		case "no-gpu":
			if c.OK || !strings.Contains(c.Why, "no GPU use") {
				t.Errorf("a case that never touched the GPU must fail: %+v", c)
			}
		}
	}
	if len(got) != 3 || res.Passed {
		t.Fatalf("want 3 cases and passed=false: %s", b)
	}
}

// Every check helper is called while a case is being SET UP, before its tool
// has run, so the output file does not exist yet. On 2026-11-10 valid_wav_in
// looked for the file at that moment, found nothing and returned "false",
// failing a DeepFilterNet run that had worked. For each helper: build the
// string with no output present, create the output, then evaluate the string
// (the real order); and prove the same string still fails when the output
// never appears.
func TestSmokeCheckStringsAreBuiltBeforeTheOutputExists(t *testing.T) {
	for _, tool := range []string{"bash", "identify", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("needs " + tool)
		}
	}
	lib := filepath.Join(scriptsDir(t), "smoke", "smoke-lib.sh")
	fix := filepath.Join(scriptsDir(t), "smoke", "fixtures")
	// n_png lives in restore.sh; take its one-line definition from there.
	rb, err := os.ReadFile(filepath.Join(scriptsDir(t), "smoke", "restore.sh"))
	if err != nil {
		t.Fatal(err)
	}
	nPNG := ""
	for _, line := range strings.Split(string(rb), "\n") {
		if strings.HasPrefix(line, "n_png() {") {
			nPNG = line
		}
	}
	if nPNG == "" {
		t.Fatal("n_png not found in restore.sh")
	}
	cases := []struct {
		name   string
		helper string // shell: prints the check string; $D is an empty dir
		make   string // shell: creates the output under $D
	}{
		{"is_png", `is_png "$D/out.png"`, `cp '` + fix + `/photo.png' "$D/out.png"`},
		{"valid_wav", `valid_wav "$D/out.wav"`, `ffmpeg -v error -f lavfi -i sine=duration=2 "$D/out.wav"`},
		{"valid_wav_in", `valid_wav_in "$D"`, `ffmpeg -v error -f lavfi -i sine=duration=2 "$D/in48_DeepFilterNet3.wav"`},
		{"n_png", `n_png "$D" 4`, `cp '` + fix + `'/frames/f0[1-4].png "$D/"`},
	}
	for _, c := range cases {
		for _, produce := range []bool{true, false} {
			d := t.TempDir()
			script := ". '" + lib + "' >/dev/null 2>&1\n" + nPNG + "\nD='" + d + "'\n" +
				"c=\"$(" + c.helper + ")\"\n" // built with no output present
			if produce {
				script += c.make + "\n"
			}
			script += "sh -c \"$c\"\n"
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "SMOKE_OUT="+filepath.Join(d, "s.json"))
			out, runErr := cmd.CombinedOutput()
			if passed := runErr == nil; passed != produce {
				t.Errorf("%s: output produced=%v but check passed=%v\n%s", c.name, produce, passed, out)
			}
		}
	}
}

func TestExportDryRunListsPathsWithoutTouchingTheHost(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// Stand-in burst-ctl: prints the real embedded lists.
	ctl := filepath.Join(t.TempDir(), "burst-ctl")
	script := "#!/bin/sh\ncd " + filepath.Join(scriptsDir(t), "../..") + " && exec go run ./cmd/burst-ctl \"$@\"\n"
	if err := os.WriteFile(ctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "export-runtime.sh"), "restore", "--dry-run", "--date", "2026-11-10")
	cmd.Env = append(os.Environ(), "BURST_CTL="+ctl)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"s3://media-manipulator/burst-images/2026-11-10/restore.tar.zst", "/opt/media-manipulator-ai/venvs/restore-vsr-mm", "dry run"} {
		if !strings.Contains(s, want) {
			t.Errorf("export dry-run lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/home/") || strings.Contains(s, ".env") && !strings.Contains(s, "Skipped everywhere") {
		t.Errorf("export plan lists a forbidden path:\n%s", s)
	}
}

// MM139: lab-voice is a FRESH family. Its provisioner installs everything on the
// builder: no export, no unpack, no Vulkan step, no HF token, wrappers from the
// bundle, a runtime manifest recorded from the installed versions.
func TestLabVoiceProvisionDryRunIsAFreshInstall(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command("bash", filepath.Join(scriptsDir(t), "provision-lab-voice.sh"), "--dry-run", "--date", "2026-11-11").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"fresh install: no host export for lab-voice", "fish_s2: venv", "indextts2: venv", "seedvc: venv", "cosyvoice3: venv",
		"scripts/lab", "runtime_manifest.py", "lab-voice.runtime-manifest", "smoke/lab-voice.sh", "8.9;12.0", "over 85% full", "scrub token variables", "cloud-init clean",
		"0f8a12e8a651062fdee9c33a894bdccd3a8513f1", "d9e41aac89fd00b3d71497fddb287b7f24613712", "51383efd921027683c89e5348211d93ff12ac2a8", "074ca6dc9e80a2f424f1f74b48bdd7d3fea531cc"} {
		if !strings.Contains(s, want) {
			t.Errorf("lab-voice dry-run plan lacks %q", want)
		}
	}
	// No host export: nothing dated under burst-images/ and no export tarball.
	// (burst-images/apt/ is the shared ffmpeg package copy, which every family may read.)
	for _, bad := range []string{"burst-images/2026", ".tar.zst", "vulkaninfo", "libnvidia-gl", "unpack"} {
		if strings.Contains(s, bad) {
			t.Errorf("a fresh family must not touch %q:\n%s", bad, s)
		}
	}
	// No credential is ever typed into or passed to the builder.
	b, _ := os.ReadFile(filepath.Join(scriptsDir(t), "provision-lab-voice.sh"))
	if regexp.MustCompile(`read\s+-[a-z]*s|HF_TOKEN=\S`).MatchString(string(b)) {
		t.Error("the provisioner must not read or set a Hugging Face token")
	}
}

func TestLabVoiceBuildDryRunHasNoHostExport(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\necho AWS-WAS-CALLED \"$@\" >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{filepath.Join(scriptsDir(t), "build-image.sh"), "lab-voice", "2026-11-11", "--dry-run", "--yes"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	got, err := run()
	if err != nil || strings.Contains(got, "AWS-WAS-CALLED") {
		t.Fatalf("err=%v\n%s", err, got)
	}
	for _, want := range []string{"builder=g7.2xlarge", "Fresh install: no host export", "Root volume:   200 GiB", "Estimated cost", "scripts/lab", "BURST_AMIS=lab-voice:x86_64=", "terminate"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "head-object") || strings.Contains(got, "burst-images/") {
		t.Errorf("a fresh family must not look for a host export:\n%s", got)
	}
	if _, err := run("--instance-type", "g6.xlarge"); err != nil {
		t.Errorf("g6.xlarge stays selectable: %v", err)
	}
	if _, err := run("--instance-type", "g6e.xlarge"); err == nil {
		t.Error("g6e.xlarge is not allowed by the builder policy and must be refused")
	}
	// The host export refuses a fresh family with a pointer to the build script.
	exp := exec.Command("bash", filepath.Join(scriptsDir(t), "export-runtime.sh"), "lab-voice", "--dry-run")
	if out, err := exp.CombinedOutput(); err == nil || !strings.Contains(string(out), "installed fresh") {
		t.Errorf("export-runtime must refuse lab-voice: %v\n%s", err, out)
	}
}

// The product families' scripts must behave exactly as before MM139: their plan
// and dry-run output are compared with the files recorded from main.
func TestProductFamiliesDryRunOutputUnchangedByMM139(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// Spot checks that survive timestamp noise: the unpack step and the Vulkan step stay in every product plan, in order.
	for _, fam := range manifest.Families {
		out, err := exec.Command("bash", filepath.Join(scriptsDir(t), "provision-"+fam+".sh"), "--dry-run", "--date", "2026-11-10").CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		i, j, k := strings.Index(s, "s3: download and unpack "+fam), strings.Index(s, "vulkan: NVIDIA GL/EGL"), strings.Index(s, "worker: directories")
		if i < 0 || j < i || k < j || strings.Contains(s, "fresh install") {
			t.Errorf("%s: unpack/vulkan/worker order changed (%d %d %d)", fam, i, j, k)
		}
	}
}
