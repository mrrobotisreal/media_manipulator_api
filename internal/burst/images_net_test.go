package burst

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// On 2026-11-11 a restore builder sat for 170 minutes (about $7 on g7.2xlarge)
// in `add-apt-repository ppa:ubuntuhandbook1/ffmpeg8`: Ubuntu's Launchpad and
// key server were unreachable, nothing in provisioning had a time limit, and
// the builder script only knew its 170-minute ceiling. These tests keep every
// download bounded, keep ffmpeg installable without Launchpad, and make the
// builder script stop when a builder goes quiet.

// fakeBin writes fake commands into a fresh directory and returns it with the
// directory the fakes record into.
func fakeBin(t *testing.T, scripts map[string]string) (bin, rec string) {
	t.Helper()
	bin, rec = t.TempDir(), t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// macOS has no coreutils `timeout`; a small stand-in with the same contract
	// (exit 124 when the limit ends the command) lets the tests run there too.
	if _, err := exec.LookPath("timeout"); err != nil {
		shim := `#!/bin/bash
case "$1" in --kill-after=*) shift ;; esac
limit="$1"; shift
"$@" <&0 &
pid=$!
( sleep "$limit"; kill -TERM "$pid" 2>/dev/null ) >/dev/null 2>&1 &
w=$!
wait "$pid"; rc=$?
if kill -0 "$w" 2>/dev/null; then kill "$w" 2>/dev/null; exit "$rc"; fi
exit 124
`
		if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, rec
}

// fakeAWS keeps the newest `aws s3 cp - s3://.../<name>` body in $MM_REC/s3/<name>
// (like S3: an upload replaces the object) and serves $MM_REC/mirror.tar as the ffmpeg copy when it exists.
const fakeAWS = `#!/bin/bash
mkdir -p "$MM_REC/s3"
echo "$*" >> "$MM_REC/aws.log"
case "$*" in
  "s3 cp - s3://"*)
    name="${4##*/}"
    cat > "$MM_REC/s3/$name"
    ;;
  "s3 cp s3://"*"ffmpeg8-noble-amd64.tar "*)
    [ -f "$MM_REC/mirror.tar" ] || { echo "An error occurred (404) when calling the HeadObject operation: Not Found" >&2; exit 1; }
    cp "$MM_REC/mirror.tar" "$4"
    ;;
  *) exit 0 ;;
esac
`

func runCommon(t *testing.T, bin, rec, script string, env ...string) (string, error, time.Duration) {
	t.Helper()
	common := filepath.Join(scriptsDir(t), "provision-common.sh")
	cmd := exec.Command("bash", "-c", `. "`+common+`"; `+script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_REC="+rec, "MM_FAMILY=restore",
		"MM_PROVISION_LOG="+filepath.Join(rec, "provision.log"))
	cmd.Env = append(cmd.Env, env...)
	t0 := time.Now()
	out, err := cmd.CombinedOutput()
	return string(out), err, time.Since(t0)
}

func recorded(rec, name string) string {
	b, _ := os.ReadFile(filepath.Join(rec, name))
	return string(b)
}

func TestNetEndsAHungCommandAndSaysSo(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	bin, rec := fakeBin(t, map[string]string{"aws": fakeAWS})
	out, err, took := runCommon(t, bin, rec, `rc=0; net 1 sleep 60 || rc=$?; echo "rc=$rc"`)
	if err != nil || !strings.Contains(out, "rc=124") {
		t.Fatalf("a hung command must end with 124: err=%v\n%s", err, out)
	}
	if took > 30*time.Second {
		t.Fatalf("net 1 took %s", took)
	}
	if st := recorded(rec, "s3/restore.status"); !strings.Contains(st, "TIMED OUT after 1s") || !strings.Contains(st, "sleep 60") {
		t.Errorf("the timeout must be reported as a status, got %q", st)
	}

	// A command that fails by itself keeps its own exit code and is not called a timeout.
	bin, rec = fakeBin(t, map[string]string{"aws": fakeAWS})
	out, err, _ = runCommon(t, bin, rec, `rc=0; net 30 sh -c 'exit 3' || rc=$?; net 30 true; echo "rc=$rc"`)
	if err != nil || !strings.Contains(out, "rc=3") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if st := recorded(rec, "s3/restore.status"); strings.Contains(st, "TIMED OUT") {
		t.Errorf("an ordinary failure was reported as a timeout: %q", st)
	}

	// MM_NET_LIMIT_CAP caps every limit.
	bin, rec = fakeBin(t, map[string]string{"aws": fakeAWS})
	out, err, took = runCommon(t, bin, rec, `rc=0; net 3600 sleep 60 || rc=$?; echo "rc=$rc"`, "MM_NET_LIMIT_CAP=1")
	if err != nil || !strings.Contains(out, "rc=124") || took > 30*time.Second {
		t.Fatalf("cap ignored: err=%v took=%s\n%s", err, took, out)
	}
}

// No provisioning script may start a download without a time limit, and none
// may use add-apt-repository (it asks api.launchpad.net and
// keyserver.ubuntu.com for the PPA key and waits for ever when they are down).
func TestProvisionDownloadsAllHaveATimeLimit(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(scriptsDir(t), "provision-*.sh"))
	if err != nil || len(files) < 6 {
		t.Fatalf("provision scripts: %v %v", files, err)
	}
	bounded := regexp.MustCompile(`(^|[\s;({|&])(net\s+\d+|timeout\s+\d+|apt_get)\s`)
	downloads := []*regexp.Regexp{
		regexp.MustCompile(`\bapt-get\s`),
		regexp.MustCompile(`\bgit\s+clone\b`),
		regexp.MustCompile(`\bsubmodule\s+update\b`),
		regexp.MustCompile(`\bhf"\s+download\b`),
		regexp.MustCompile(`\buv"\s+(sync|venv|pip\s+install)\b`),
		regexp.MustCompile(`\bpip"?\s+install\b`),
		regexp.MustCompile(`\baws\s+s3\s+cp\b`),
	}
	curl := regexp.MustCompile(`(^|[;&|(]\s*)curl\s`)
	curlBounded := regexp.MustCompile(`CURL_NET|\s(-m|--max-time)\s`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			tl := strings.TrimSpace(line)
			// Comments, plan/status text and the two helper definitions are words, not calls.
			if tl == "" || strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "plan \"") || strings.HasPrefix(tl, "status \"") ||
				strings.HasPrefix(tl, "apt_get() {") || strings.HasPrefix(tl, "CURL_NET=") {
				continue
			}
			where := filepath.Base(f) + ":" + itoa(i+1)
			if strings.Contains(tl, "add-apt-repository") {
				t.Errorf("%s uses add-apt-repository (it waits on Launchpad and the key server): %s", where, tl)
			}
			if curl.MatchString(tl) && !curlBounded.MatchString(tl) {
				t.Errorf("%s: curl without a time limit (use \"${CURL_NET[@]}\" or -m): %s", where, tl)
			}
			// The one pip call that downloads nothing: a source build of an already
			// cloned tree with no dependency resolution and no build isolation. It
			// is a long compile, reported by with_heartbeat instead of being limited.
			localBuild := strings.Contains(tl, "--no-build-isolation") && strings.Contains(tl, "--no-deps") && strings.Contains(tl, "with_heartbeat ")
			for _, re := range downloads {
				if re.MatchString(tl) && !bounded.MatchString(tl) && !localBuild {
					t.Errorf("%s: a download without a time limit (wrap it in `net SECONDS` or use apt_get): %s", where, tl)
				}
			}
		}
	}
	// The builder's own bootstrap (user-data in build-image.sh) is held to the same rule.
	b, err := os.ReadFile(filepath.Join(scriptsDir(t), "build-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, `USERDATA="#!/bin/bash`), strings.Index(s, "# 5. Launch")
	if i < 0 || j < i {
		t.Fatal("user-data block not found in build-image.sh")
	}
	for _, line := range strings.Split(s[i:j], "\n") {
		for _, part := range strings.Split(line, "|") {
			if strings.Contains(part, "aws s3 cp") && !strings.Contains(part, "timeout ") {
				t.Errorf("user-data: aws s3 cp without a time limit: %s", strings.TrimSpace(part))
			}
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// step_ffmpeg against fake apt/aws/ffmpeg: the S3 copy is used first, the PPA
// is the fallback, and neither a hanging nor a silently missing source can
// hold the builder.
func TestStepFfmpegNeverWaitsOnADeadSource(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// apt-get: records its arguments and the ffmpeg source line in force; with
	// MM_FAKE_APT=hang an `update` that includes Launchpad never returns.
	aptGet := `#!/bin/bash
src="$(cat "$MM_APT_LIST_DIR/mm-ffmpeg8.list" 2>/dev/null)"
echo "ARGS=$* | SRC=$src" >> "$MM_REC/apt.log"
case " $* " in *" update "*)
  if [ "${MM_FAKE_APT:-}" = hang ]; then case "$src" in *launchpadcontent*) exec sleep 300 ;; esac; fi ;;
esac
exit 0
`
	// apt-cache policy: the candidate depends on which source is configured.
	aptCache := `#!/bin/bash
src="$(cat "$MM_APT_LIST_DIR/mm-ffmpeg8.list" 2>/dev/null)"
case "$src" in
  *file:*) c="${MM_CAND_COPY:-7:6.1.1-3ubuntu5}" ;;
  *launchpadcontent*) c="${MM_CAND_PPA:-7:6.1.1-3ubuntu5}" ;;
  *) c="7:6.1.1-3ubuntu5" ;;
esac
printf 'ffmpeg:\n  Installed: (none)\n  Candidate: %s\n  Version table:\n' "$c"
`
	ffmpeg := `#!/bin/bash
case "$*" in
  *-version*) echo "ffmpeg version 8.1.2-0build1~ubuntu24.04 Copyright (c) 2000-2026 the FFmpeg developers" ;;
  *-encoders*) echo " V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)" ;;
esac
`
	addRepo := "#!/bin/bash\ntouch \"$MM_REC/ADD-APT-REPOSITORY-CALLED\"\nexec sleep 300\n"

	type tc struct {
		name      string
		copyInS3  bool
		env       []string
		wantOK    bool
		wantText  []string // in status + failed
		wantNoPPA bool     // Launchpad never appears in a source line
		wantList  string   // "" = removed; else a substring the kept source line holds
	}
	cases := []tc{
		{name: "the S3 copy is used and Launchpad is never contacted", copyInS3: true, env: []string{"MM_CAND_COPY=10:8.1.2-0build1~ubuntu24.04"},
			wantOK: true, wantText: []string{"ffmpeg 8.1.2-0build1~ubuntu24.04 installed from the S3 copy"}, wantNoPPA: true},
		{name: "no copy: the PPA is used with the committed key", env: []string{"MM_CAND_PPA=10:8.1.2-0build1~ubuntu24.04"},
			wantOK: true, wantText: []string{"no usable ffmpeg copy in S3", "installed from the PPA"}, wantList: "signed-by="},
		{name: "a copy apt does not accept falls back to the PPA", copyInS3: true, env: []string{"MM_CAND_PPA=10:8.1.2-0build1~ubuntu24.04"},
			wantOK: true, wantText: []string{"installed from the PPA"}, wantList: "launchpadcontent"},
		{name: "no copy and Launchpad hangs: fails in seconds", env: []string{"MM_FAKE_APT=hang", "MM_NET_LIMIT_CAP=2"},
			wantText: []string{"TIMED OUT after 2s", "apt-get update failed or timed out after adding the ffmpeg 8 source"}},
		{name: "no copy and the PPA silently did not load: says so", env: nil,
			wantText: []string{"the ffmpeg 8 package source did not load", "7:6.1.1-3ubuntu5", "mirror-ffmpeg.sh"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, rec := fakeBin(t, map[string]string{"aws": fakeAWS, "apt-get": aptGet, "apt-cache": aptCache, "ffmpeg": ffmpeg, "add-apt-repository": addRepo})
			work := t.TempDir()
			lists, keys, mirror := filepath.Join(work, "lists"), filepath.Join(work, "keys"), filepath.Join(work, "apt")
			if c.copyInS3 {
				src := filepath.Join(work, "src", "ubuntu", "dists", "noble")
				if err := os.MkdirAll(src, 0o755); err != nil {
					t.Fatal(err)
				}
				_ = os.WriteFile(filepath.Join(src, "InRelease"), []byte("signed index"), 0o644)
				if out, err := exec.Command("tar", "-C", filepath.Join(work, "src"), "-cf", filepath.Join(rec, "mirror.tar"), "ubuntu").CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			env := append([]string{"MM_APT_LIST_DIR=" + lists, "MM_KEYRING_DIR=" + keys, "MM_APT_MIRROR_DIR=" + mirror}, c.env...)
			out, err, took := runCommon(t, bin, rec, "step_ffmpeg", env...)
			said := recorded(rec, "s3/restore.status") + recorded(rec, "s3/restore.failed")
			if (err == nil) != c.wantOK {
				t.Fatalf("ok=%v want %v\n%s\n%s", err == nil, c.wantOK, out, said)
			}
			if took > 60*time.Second {
				t.Fatalf("took %s: a dead source must never hold the builder", took)
			}
			for _, w := range c.wantText {
				if !strings.Contains(said, w) {
					t.Errorf("missing %q in:\n%s", w, said)
				}
			}
			aptLog := recorded(rec, "apt.log")
			if c.wantNoPPA && strings.Contains(aptLog, "launchpad") {
				t.Errorf("Launchpad was used although the S3 copy works:\n%s", aptLog)
			}
			if _, e := os.Stat(filepath.Join(rec, "ADD-APT-REPOSITORY-CALLED")); e == nil {
				t.Error("add-apt-repository was called")
			}
			for _, line := range strings.Split(strings.TrimSpace(aptLog), "\n") {
				if !strings.Contains(line, "Acquire::http::Timeout=30") || !strings.Contains(line, "Acquire::Retries=3") {
					t.Errorf("apt-get ran without its timeouts: %s", line)
				}
			}
			if c.wantOK {
				if !strings.Contains(aptLog, "install -y -qq ffmpeg") {
					t.Errorf("ffmpeg was not installed:\n%s", aptLog)
				}
				// The committed key is what verifies the source, whichever one is used.
				k, e := os.ReadFile(filepath.Join(keys, "mm-ffmpeg8.asc"))
				if e != nil || !strings.Contains(string(k), "BEGIN PGP PUBLIC KEY BLOCK") {
					t.Errorf("signing key not installed: %v", e)
				}
				list, e := os.ReadFile(filepath.Join(lists, "mm-ffmpeg8.list"))
				if c.wantList == "" {
					if e == nil {
						t.Errorf("the image must not keep a source line that points at the build-time copy: %s", list)
					}
					if _, e2 := os.Stat(filepath.Join(mirror, "ffmpeg8")); e2 == nil {
						t.Error("the unpacked copy was left in the image")
					}
				} else if e != nil || !strings.Contains(string(list), c.wantList) {
					t.Errorf("source line = %q (%v), want it to hold %q", list, e, c.wantList)
				}
			}
		})
	}
}

// A builder whose status stops changing is stuck. The builder script must stop
// and terminate it, not wait out its 170-minute ceiling.
func TestBuildImageStopsWhenTheBuilderGoesQuiet(t *testing.T) {
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
  *"s3api head-object"*manifest.json*) exit 0 ;;
  *"s3api head-object"*) exit 1 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *"s3 cp s3://"*".status -"*) printf '12:39:20 apt: base packages\n12:39:25 ffmpeg 8.1.2 installed from the S3 copy (host has 8.1.1)\n12:39:27 apt: ffmpeg 8.1.x\n' ;;
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
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--yes", "--instance-type", "g7.2xlarge")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_POLL_SECONDS=1", "MM_STALL_SECONDS=3", "MM_REC="+rec)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("build-image.sh kept waiting for a builder that reports nothing")
	}
	got := string(out)
	if err == nil {
		t.Fatalf("must fail:\n%s", got)
	}
	for _, want := range []string{"the builder has reported nothing new for", "last status: 12:39:27 apt: ffmpeg 8.1.x", "terminating builder i-0builder"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Every status line the builder wrote is shown once, also the ones written
	// between two polls (on 2026-11-11 the line naming the ffmpeg source was lost).
	for _, line := range []string{"builder: 12:39:20 apt: base packages", "builder: 12:39:25 ffmpeg 8.1.2 installed from the S3 copy", "builder: 12:39:27 apt: ffmpeg 8.1.x"} {
		if n := strings.Count(got, line); n != 1 {
			t.Errorf("%q shown %d times, want once:\n%s", line, n, got)
		}
	}
	if term := recorded(rec, "terminated"); !strings.Contains(term, "i-0builder") {
		t.Errorf("the stuck builder was not terminated (got %q):\n%s", term, got)
	}
	if strings.Contains(got, "UNEXPECTED") {
		t.Errorf("unexpected aws call:\n%s", got)
	}
	// The dry-run plan states the rule.
	dry, derr := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--dry-run", "--yes").CombinedOutput()
	if derr != nil || !strings.Contains(string(dry), "when its status has not changed for 90 min") {
		t.Errorf("dry-run plan lacks the stall rule: %v\n%s", derr, dry)
	}
}

// mirror-ffmpeg.sh copies only what the PPA's key vouches for: the index must be
// signed by the committed key, and every list and package must match the index.
func TestMirrorFfmpegCopiesOnlyWhatThePPAKeySigned(t *testing.T) {
	for _, tool := range []string{"bash", "gpg", "gpgv", "curl", "sha256sum", "gunzip", "gzip", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	// A short path: gpg-agent's socket lives in the home dir and has a length limit.
	root, err := os.MkdirTemp("/tmp", "mmg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", filepath.Join(root, "a"), "--kill", "gpg-agent").Run()
		_ = exec.Command("gpgconf", "--homedir", filepath.Join(root, "b"), "--kill", "gpg-agent").Run()
		_ = os.RemoveAll(root)
	})
	sh := func(script string, env ...string) (string, error) {
		cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+script)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, k := range []string{"a", "b"} {
		if out, err := sh(`mkdir -m 700 ` + k + `
gpg --homedir ` + k + ` --batch --quiet --passphrase '' --pinentry-mode loopback --quick-gen-key "mm test ` + k + `" ed25519 sign never
gpg --homedir ` + k + ` --batch --armor --export > ` + k + `.asc`); err != nil {
			t.Skipf("gpg cannot make a test key here: %v\n%s", err, out)
		}
	}
	// build NAME VERSION: a one-package repository signed by key a.
	build := `build() {
  local r="$1/ubuntu" v="$2"
  mkdir -p "$r/dists/noble/main/binary-amd64" "$r/pool/main/f/ffmpeg"
  printf 'not really a deb %s' "$v" > "$r/pool/main/f/ffmpeg/ffmpeg_test_amd64.deb"
  local d="$r/pool/main/f/ffmpeg/ffmpeg_test_amd64.deb" p="$r/dists/noble/main/binary-amd64/Packages"
  printf 'Package: ffmpeg\nVersion: %s\nArchitecture: amd64\nFilename: pool/main/f/ffmpeg/ffmpeg_test_amd64.deb\nSize: %s\nSHA256: %s\nDescription: test\n' \
    "$v" "$(wc -c < "$d" | tr -d ' ')" "$(sha256sum "$d" | cut -d' ' -f1)" > "$p"
  gzip -n -c "$p" > "$p.gz"
  { printf 'Origin: test\nSuite: noble\nDate: Sat, 20 Jun 2026 15:04:41 UTC\nSHA256:\n'
    for f in main/binary-amd64/Packages main/binary-amd64/Packages.gz; do
      printf ' %s %s %s\n' "$(sha256sum "$r/dists/noble/$f" | cut -d' ' -f1)" "$(wc -c < "$r/dists/noble/$f" | tr -d ' ')" "$f"
    done
  } > "$r/dists/noble/Release"
  # Like Launchpad: the uncompressed list is named in the index but not served.
  rm -f "$p"
  gpg --homedir a --batch --quiet --passphrase '' --pinentry-mode loopback --clearsign -o "$r/dists/noble/InRelease" "$r/dists/noble/Release"
}
`
	mirror := filepath.Join(scriptsDir(t), "mirror-ffmpeg.sh")
	run := func(repo, key, prep string) (string, error) {
		return sh(build+prep+`
bash "`+mirror+`" --no-upload --out "$PWD/`+repo+`.tar" 2>&1`,
			"MM_FFMPEG_PPA_URL=file://"+filepath.Join(root, repo, "ubuntu"), "MM_FFMPEG_KEY_FILE="+filepath.Join(root, key),
			// The test repository's one "package" is a text file, so apt's own check of
			// the copy (step 4) is switched off here; it runs for real on the server.
			"MM_MIRROR_APT_SELFTEST=0")
	}

	// Good: signed by the key, everything matches.
	out, err := run("good", "a.asc", `build good 10:8.1.2-0build1~ubuntu24.04`)
	if err != nil {
		t.Fatalf("a good repository must be copied: %v\n%s", err, out)
	}
	for _, want := range []string{"MIRROR RESULT", "ffmpeg version:  10:8.1.2-0build1~ubuntu24.04", "signature:       good", "packages:        1 ", "not uploaded"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	list, err := exec.Command("tar", "-tf", filepath.Join(root, "good.tar")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, list)
	}
	for _, want := range []string{"ubuntu/dists/noble/InRelease", "ubuntu/dists/noble/main/binary-amd64/Packages.gz", "ubuntu/dists/noble/main/binary-amd64/Packages\n",
		"ubuntu/pool/main/f/ffmpeg/ffmpeg_test_amd64.deb", "ubuntu/MIRROR.json"} {
		if !strings.Contains(string(list)+"\n", want) {
			t.Errorf("the tar lacks %q:\n%s", strings.TrimSpace(want), list)
		}
	}

	bad := []struct{ name, repo, key, prep, want string }{
		{"an index signed by another key", "otherkey", "b.asc", `build otherkey 10:8.1.2-0build1~ubuntu24.04`, "NOT signed by the PPA's key"},
		{"a package changed after signing", "baddeb", "a.asc", `build baddeb 10:8.1.2-0build1~ubuntu24.04
printf 'swapped' > baddeb/ubuntu/pool/main/f/ffmpeg/ffmpeg_test_amd64.deb`, "does not match the signed package list"},
		{"a package list changed after signing", "badlist", "a.asc", `build badlist 10:8.1.2-0build1~ubuntu24.04
printf 'Package: evil\n' | gzip -n -c > badlist/ubuntu/dists/noble/main/binary-amd64/Packages.gz`, "does not match the signed index"},
		{"an index edited after signing", "badindex", "a.asc", `build badindex 10:8.1.2-0build1~ubuntu24.04
sed -i.bak 's/^Suite: noble/Suite: nobla/' badindex/ubuntu/dists/noble/InRelease`, "NOT signed by the PPA's key"},
		{"a different ffmpeg series", "ff9", "a.asc", `build ff9 10:9.0-0build1~ubuntu24.04`, "not an 8.1.x build"},
		// A file gpg can read without any signature on it must not pass as "verified".
		{"an index with no signature at all", "nosig", "a.asc", `build nosig 10:8.1.2-0build1~ubuntu24.04
rm nosig/ubuntu/dists/noble/InRelease
gpg --homedir a --batch --quiet --armor --store -o nosig/ubuntu/dists/noble/InRelease nosig/ubuntu/dists/noble/Release`, "NOT signed by the PPA's key"},
	}
	for _, c := range bad {
		out, err := run(c.repo, c.key, c.prep)
		if err == nil {
			t.Errorf("%s: must be refused:\n%s", c.name, out)
			continue
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: missing %q:\n%s", c.name, c.want, out)
		}
		if strings.Contains(out, "MIRROR RESULT") {
			t.Errorf("%s: printed a result box:\n%s", c.name, out)
		}
		if _, e := os.Stat(filepath.Join(root, c.repo+".tar")); e == nil {
			t.Errorf("%s: a tar was written", c.name)
		}
	}
}

// step_smoke reports each finished case as a status (so a long smoke run is
// never mistaken for a stuck builder), keeps the smoke script's exit code and
// passes its output on to the provision log.
func TestStepSmokeReportsEachCaseAndKeepsTheExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, c := range []struct {
		name   string
		exit   string
		wantOK bool
	}{{"all pass", "0", true}, {"a case fails", "1", false}} {
		t.Run(c.name, func(t *testing.T) {
			// A copy of the two sourced scripts beside a fake smoke script.
			dir := t.TempDir()
			for _, f := range []string{"provision-common.sh", "lib.sh"} {
				b, err := os.ReadFile(filepath.Join(scriptsDir(t), f))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, f), b, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(dir, "smoke"), 0o755); err != nil {
				t.Fatal(err)
			}
			smoke := `#!/bin/bash
printf '%-22s %s  %6.2fs  gpu+%s MiB  %s\n' rembg PASS 3.10 812 "" >&2
sleep 2
printf '%-22s %s  %6.2fs  gpu+%s MiB  %s\n' upscale FAIL 1.20 0 "exit 1" >&2
echo "a plain line" >&2
echo '{"passed":false}' > "$SMOKE_OUT"
exit ` + c.exit + "\n"
			if err := os.WriteFile(filepath.Join(dir, "smoke", "fake.sh"), []byte(smoke), 0o755); err != nil {
				t.Fatal(err)
			}
			bin, rec := fakeBin(t, map[string]string{"aws": fakeAWS})
			cmd := exec.Command("bash", "-c", `. "`+filepath.Join(dir, "provision-common.sh")+`"; step_smoke; echo SMOKE-RETURNED`)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MM_REC="+rec, "MM_FAMILY=fake",
				"MM_PROVISION_LOG="+filepath.Join(rec, "provision.log"), "MM_SMOKE_POLL_SECONDS=1")
			out, err := cmd.CombinedOutput()
			got, st := string(out), recorded(rec, "s3/fake.status")
			if (err == nil) != c.wantOK || strings.Contains(got, "SMOKE-RETURNED") != c.wantOK {
				t.Fatalf("ok=%v want %v\n%s", err == nil, c.wantOK, got)
			}
			for _, want := range []string{"smoke: rembg PASS", "smoke: upscale FAIL"} {
				if !strings.Contains(st, want) {
					t.Errorf("status lacks %q:\n%s", want, st)
				}
			}
			if strings.Contains(st, "a plain line") {
				t.Errorf("only case results are statuses:\n%s", st)
			}
			if !strings.Contains(got, "a plain line") {
				t.Errorf("the smoke script's output must reach the provision log:\n%s", got)
			}
			if !c.wantOK && !strings.Contains(recorded(rec, "s3/fake.failed"), "smoke test failed") {
				t.Errorf("a failed smoke run must fail the build:\n%s", got)
			}
			if !strings.Contains(recorded(rec, "aws.log"), "fake.smoke.json") {
				t.Errorf("the smoke json was not uploaded:\n%s", recorded(rec, "aws.log"))
			}
		})
	}
}

// A long local compile reports while it runs and keeps its exit code; the
// reporter stops with it.
func TestWithHeartbeatReportsAndKeepsTheExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	bin, rec := fakeBin(t, map[string]string{"aws": fakeAWS})
	out, err, _ := runCommon(t, bin, rec,
		`rc=0; with_heartbeat "mmcv build" sh -c 'sleep 3; exit 7' || rc=$?; echo "rc=$rc"; sleep 2; echo "after=$(grep -c 'still running' "$MM_REC/s3/restore.status")"`,
		"MM_HEARTBEAT_SECONDS=1")
	if err != nil || !strings.Contains(out, "rc=7") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	st := recorded(rec, "s3/restore.status")
	n := strings.Count(st, "still running: mmcv build")
	if n < 1 {
		t.Fatalf("no heartbeat status:\n%s", st)
	}
	// The count printed 2 s after the command ended equals the final count: the reporter stopped.
	if !strings.Contains(out, "after="+itoa(n)) {
		t.Errorf("the heartbeat kept running after its command ended (final %d):\n%s", n, out)
	}
}
